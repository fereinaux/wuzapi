package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

func webhookFormatIsJSON() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("WEBHOOK_FORMAT")), "json")
}

func webhookSubscriptionAllows(mycli *MyClient, eventType string) bool {
	if len(mycli.subscriptions) == 0 {
		return false
	}
	if Find(mycli.subscriptions, "All") {
		return true
	}
	return Find(mycli.subscriptions, eventType)
}

func (s *server) dispatchWebhook(mycli *MyClient, postmap map[string]interface{}) {
	if mycli == nil || s == nil {
		return
	}
	ui, ok := userinfocache.Get(mycli.token)
	if !ok {
		return
	}
	v := ui.(Values)
	if v.Get("InboundWebhook") != "true" {
		return
	}
	wh := strings.TrimSpace(v.Get("Webhook"))
	if wh == "" {
		return
	}
	eventType, _ := postmap["type"].(string)
	if eventType != "" && !webhookSubscriptionAllows(mycli, eventType) {
		return
	}

	payload := make(map[string]interface{}, len(postmap)+4)
	for k, val := range postmap {
		payload[k] = val
	}
	payload["userID"] = mycli.userID
	payload["instanceName"] = v.Get("Name")
	if webhookFormatIsJSON() {
		payload["token"] = mycli.token
	}

	var body []byte
	var contentType string
	var err error
	if webhookFormatIsJSON() {
		body, err = json.Marshal(payload)
		contentType = "application/json; charset=utf-8"
	} else {
		var jsonData []byte
		jsonData, err = json.Marshal(payload)
		if err == nil {
			form := url.Values{}
			form.Set("jsonData", string(jsonData))
			form.Set("token", mycli.token)
			body = []byte(form.Encode())
			contentType = "application/x-www-form-urlencoded"
		}
	}
	if err != nil {
		log.Error().Err(err).Msg("webhook marshal failed")
		return
	}

	// HistorySync de conta grande pode levar muito mais que 30s no consumidor (parse + INSERT
	// em lotes). Timeout maior evita perda silenciosa do payload, já que aqui não há retry.
	timeout := 120 * time.Second
	if envVal := strings.TrimSpace(os.Getenv("WEBHOOK_TIMEOUT_SECONDS")); envVal != "" {
		if n, err := strconv.Atoi(envVal); err == nil && n > 0 && n <= 600 {
			timeout = time.Duration(n) * time.Second
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh, bytes.NewReader(body))
	if err != nil {
		log.Error().Err(err).Msg("webhook request")
		return
	}
	req.Header.Set("Content-Type", contentType)

	hmacB64 := v.Get("HmacKeyEncrypted")
	if hmacB64 != "" {
		encrypted, derr := base64.StdEncoding.DecodeString(hmacB64)
		if derr == nil && len(encrypted) > 0 {
			sig, serr := generateHmacSignature(body, encrypted)
			if serr != nil {
				log.Warn().Err(serr).Msg("hmac signature")
			} else if sig != "" {
				req.Header.Set("x-hmac-signature", sig)
			}
		}
	}

	resp, err := webhookHTTPClient.Do(req)
	if err != nil {
		log.Warn().Err(err).Str("url", wh).Msg("webhook post failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slurp, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		log.Warn().Int("status", resp.StatusCode).Str("body", string(slurp)).Msg("webhook non-success status")
	}
}
