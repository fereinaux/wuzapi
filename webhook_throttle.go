package main

import (
	"sync"
	"time"
)

/*
Throttle de webhooks por usuário (Fase 5.1 do plano anti-banimento do consumidor).

Por que: o whatsmeow despacha webhooks via goroutines independentes. Em rajada
(ex.: HistorySync de conta grande emite N chunks + ReadReceipts simultâneos), o
consumidor pode receber dezenas de POSTs paralelos pro mesmo usuário, gerando
locks e contenção.

Estratégia: serializa por userID com intervalo mínimo entre webhooks (default 50ms,
configurável via WEBHOOK_MIN_INTERVAL_MS). NÃO descarta eventos — só espaça.
Aplicar antes do http.Do.
*/

var (
	webhookPacingMu   sync.Mutex
	webhookLastByUser = make(map[string]time.Time)
	webhookMinIntMs   = 50
)

// waitForWebhookSlot bloqueia até que tenha passado o intervalo mínimo desde o
// último webhook deste userID. Atualiza o timestamp ao retornar.
func waitForWebhookSlot(userID string) {
	if userID == "" {
		return
	}
	webhookPacingMu.Lock()
	last, ok := webhookLastByUser[userID]
	now := time.Now()
	minDur := time.Duration(webhookMinIntMs) * time.Millisecond
	var wait time.Duration
	if ok {
		elapsed := now.Sub(last)
		if elapsed < minDur {
			wait = minDur - elapsed
		}
	}
	// Reserva o slot avançado pelo tempo de espera para que webhooks empilhados
	// herdem o pacing corretamente.
	webhookLastByUser[userID] = now.Add(wait)
	// Garbage collection simples: descarta entradas mais antigas que 10min para
	// não vazar memória com usuários inativos.
	if len(webhookLastByUser) > 1024 {
		threshold := now.Add(-10 * time.Minute)
		for k, v := range webhookLastByUser {
			if v.Before(threshold) {
				delete(webhookLastByUser, k)
			}
		}
	}
	webhookPacingMu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
}
