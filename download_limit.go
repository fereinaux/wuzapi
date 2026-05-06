package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/sync/semaphore"
)

var (
	globalDownloadSem      *semaphore.Weighted
	perUserDownloadMax     int
	downloadAcquireTimeout = 30 * time.Second
	userDownloadSemaphores sync.Map // userid -> *semaphore.Weighted
)

func initDownloadLimits() {
	n := 8
	if v := strings.TrimSpace(os.Getenv("WUZAPI_DOWNLOAD_MAX_CONCURRENT")); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 {
			n = x
		}
	}
	globalDownloadSem = semaphore.NewWeighted(int64(n))

	perUserDownloadMax = 0
	if v := strings.TrimSpace(os.Getenv("WUZAPI_DOWNLOAD_MAX_PER_USER")); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 {
			perUserDownloadMax = x
		}
	}
	log.Info().
		Int("global_max_concurrent", n).
		Int("per_user_max", perUserDownloadMax).
		Msg("download concurrency limits initialized")
}

// acquireDownloadSlots limita POST /chat/download (global + opcional por usuário).
func acquireDownloadSlots(ctx context.Context, userID string) (release func(), err error) {
	if globalDownloadSem == nil {
		return func() {}, nil
	}
	acqCtx, cancel := context.WithTimeout(ctx, downloadAcquireTimeout)
	defer cancel()
	if err := globalDownloadSem.Acquire(acqCtx, 1); err != nil {
		return nil, err
	}
	releasedGlobal := false
	releaseGlobal := func() {
		if !releasedGlobal {
			globalDownloadSem.Release(1)
			releasedGlobal = true
		}
	}
	if perUserDownloadMax <= 0 {
		return releaseGlobal, nil
	}
	iface, _ := userDownloadSemaphores.LoadOrStore(userID, semaphore.NewWeighted(int64(perUserDownloadMax)))
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
