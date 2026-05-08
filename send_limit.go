package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/sync/semaphore"
)

// Limite de concorrência para POST /chat/send/* e rotas correlatas (react, delete, edit, etc.).
// Espelha download_limit.go: semáforo global + opcional por usuário. Tráfego com header
// X-Wuzapi-Traffic-Class: bulk usa teto por usuário mais baixo (disparo em massa).
var (
	sendLimitingEnabled     bool
	globalSendSem           *semaphore.Weighted
	sendPerUserInteractive  int
	sendPerUserBulk         int
	sendAcquireTimeout      = 30 * time.Second
	userSendSemInteractive  sync.Map // userID -> *semaphore.Weighted
	userSendSemBulk         sync.Map // userID -> *semaphore.Weighted
)

const sendTrafficClassHeader = "X-Wuzapi-Traffic-Class"

func initSendLimits() {
	n := 0
	if v := strings.TrimSpace(os.Getenv("WUZAPI_SEND_MAX_CONCURRENT")); v != "" {
		if x, err := strconv.Atoi(v); err == nil {
			n = x
		}
	}
	if n <= 0 {
		sendLimitingEnabled = false
		log.Info().Msg("send concurrency limits disabled (set WUZAPI_SEND_MAX_CONCURRENT>0 to enable)")
		return
	}
	sendLimitingEnabled = true
	globalSendSem = semaphore.NewWeighted(int64(n))

	sendPerUserInteractive = 12
	if v := strings.TrimSpace(os.Getenv("WUZAPI_SEND_MAX_PER_USER")); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x >= 0 {
			sendPerUserInteractive = x
		}
	}

	sendPerUserBulk = 3
	if v := strings.TrimSpace(os.Getenv("WUZAPI_SEND_MAX_PER_USER_BULK")); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x >= 0 {
			sendPerUserBulk = x
		}
	}

	if v := strings.TrimSpace(os.Getenv("WUZAPI_SEND_ACQUIRE_TIMEOUT_MS")); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x >= 100 {
			sendAcquireTimeout = time.Duration(x) * time.Millisecond
		}
	}

	log.Info().
		Bool("enabled", true).
		Int("global_max_concurrent", n).
		Int("per_user_interactive", sendPerUserInteractive).
		Int("per_user_bulk", sendPerUserBulk).
		Dur("acquire_timeout", sendAcquireTimeout).
		Msg("send concurrency limits initialized")
}

func isBulkTrafficClass(r *http.Request) bool {
	v := strings.TrimSpace(r.Header.Get(sendTrafficClassHeader))
	return strings.EqualFold(v, "bulk")
}

// acquireSendSlots limita envios ao whatsmeow (global + por usuário; bulk mais restrito).
func acquireSendSlots(ctx context.Context, userID string, bulk bool) (release func(), err error) {
	if !sendLimitingEnabled || globalSendSem == nil {
		return func() {}, nil
	}
	acqCtx, cancel := context.WithTimeout(ctx, sendAcquireTimeout)
	defer cancel()
	if err := globalSendSem.Acquire(acqCtx, 1); err != nil {
		return nil, err
	}
	releasedGlobal := false
	releaseGlobal := func() {
		if !releasedGlobal {
			globalSendSem.Release(1)
			releasedGlobal = true
		}
	}
	perUserMax := sendPerUserInteractive
	userSemMap := &userSendSemInteractive
	if bulk {
		perUserMax = sendPerUserBulk
		userSemMap = &userSendSemBulk
	}
	if perUserMax <= 0 {
		return releaseGlobal, nil
	}
	if userID == "" {
		userID = "_"
	}
	iface, _ := userSemMap.LoadOrStore(userID, semaphore.NewWeighted(int64(perUserMax)))
	userSem := iface.(*semaphore.Weighted)
	if err := userSem.Acquire(acqCtx, 1); err != nil {
		releaseGlobal()
		return nil, err
	}
	releasedUser := false
	return func() {
		if !releasedUser {
			userSem.Release(1)
			releasedUser = true
		}
		releaseGlobal()
	}, nil
}

func limitConcurrentChatSends(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sendLimitingEnabled {
			next.ServeHTTP(w, r)
			return
		}
		ui := r.Context().Value("userinfo")
		if ui == nil {
			next.ServeHTTP(w, r)
			return
		}
		userID := ui.(Values).Get("Id")
		bulk := isBulkTrafficClass(r)
		release, err := acquireSendSlots(r.Context(), userID, bulk)
		if err != nil {
			log.Warn().Err(err).Str("userid", userID).Bool("bulk", bulk).
				Str("path", r.URL.Path).
				Msg("chat send: concurrency slot acquire failed")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    http.StatusServiceUnavailable,
				"success": false,
				"error":   "send capacity saturated, retry",
			})
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}
