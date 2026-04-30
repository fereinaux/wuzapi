package main

import (
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog/log"
)

func (s *server) startMidnightDisconnectScheduler() {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		log.Error().Err(err).Msg("failed to load timezone America/Sao_Paulo for midnight disconnect")
		return
	}
	go func() {
		for {
			now := time.Now().In(loc)
			next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, loc)
			d := time.Until(next)
			log.Info().Dur("until_run", d).Msg("midnight disconnect scheduler sleeping")
			time.Sleep(d)
			s.disconnectUsersWithoutHistorySyncAtMidnight()
		}
	}()
}

func (s *server) disconnectUsersWithoutHistorySyncAtMidnight() {
	var ids []string
	err := sqlx.Select(s.db, &ids, `SELECT id FROM users WHERE connected = 1 AND COALESCE(sync_history, 0) = 0`)
	if err != nil {
		log.Error().Err(err).Msg("midnight disconnect: query users")
		return
	}
	for _, id := range ids {
		if ch, ok := killchannel[id]; ok {
			select {
			case ch <- true:
			default:
			}
		} else {
			if _, err := s.db.Exec(`UPDATE users SET connected = 0, qrcode = '' WHERE id = $1`, id); err != nil {
				log.Warn().Err(err).Str("id", id).Msg("midnight disconnect: db update")
			}
		}
	}
	if len(ids) > 0 {
		log.Info().Int("count", len(ids)).Msg("midnight disconnect: signaled users without sync_history")
	}
}
