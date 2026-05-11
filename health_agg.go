package main

import (
	"sync"
	"time"
)

// Intervalo para reaproximar COUNT(users) + contagens por sessão sem martelar DB/mutex a cada GET /health.
const healthAggregatedCacheTTL = 5 * time.Second

var healthAgg struct {
	mu                sync.Mutex
	cachedAt          time.Time
	totalUsers        int
	activeConnections int
	connectedUsers    int
	loggedInUsers     int
}

func (s *server) healthAggregatedCounts() (totalUsers, activeConnections, connectedUsers, loggedInUsers int) {
	healthAgg.mu.Lock()
	defer healthAgg.mu.Unlock()
	if !healthAgg.cachedAt.IsZero() && time.Since(healthAgg.cachedAt) < healthAggregatedCacheTTL {
		return healthAgg.totalUsers, healthAgg.activeConnections, healthAgg.connectedUsers, healthAgg.loggedInUsers
	}
	tu := 0
	rows, err := s.db.Query("SELECT COUNT(*) FROM users")
	if err == nil {
		if rows.Next() {
			_ = rows.Scan(&tu)
		}
		rows.Close()
	}
	clientManager.RLock()
	ac := len(clientManager.whatsmeowClients)
	cu := 0
	lu := 0
	for _, c := range clientManager.whatsmeowClients {
		if c != nil {
			if c.IsConnected() {
				cu++
			}
			if c.IsLoggedIn() {
				lu++
			}
		}
	}
	clientManager.RUnlock()
	healthAgg.totalUsers = tu
	healthAgg.activeConnections = ac
	healthAgg.connectedUsers = cu
	healthAgg.loggedInUsers = lu
	healthAgg.cachedAt = time.Now()
	return tu, ac, cu, lu
}
