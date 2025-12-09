package main

import (
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	"go.mau.fi/whatsmeow"
)

type ClientManager struct {
	sync.RWMutex
	whatsmeowClients map[string]*whatsmeow.Client
	httpClients      map[string]*resty.Client
	myClients        map[string]*MyClient
	lastActivity     map[string]time.Time // Track last activity time for each session
}

func NewClientManager() *ClientManager {
	return &ClientManager{
		whatsmeowClients: make(map[string]*whatsmeow.Client),
		httpClients:      make(map[string]*resty.Client),
		myClients:        make(map[string]*MyClient),
		lastActivity:     make(map[string]time.Time),
	}
}

func (cm *ClientManager) SetWhatsmeowClient(userID string, client *whatsmeow.Client) {
	cm.Lock()
	defer cm.Unlock()
	cm.whatsmeowClients[userID] = client
	cm.lastActivity[userID] = time.Now()
}

func (cm *ClientManager) GetWhatsmeowClient(userID string) *whatsmeow.Client {
	cm.Lock()
	defer cm.Unlock()
	// Update last activity when client is accessed
	if _, exists := cm.whatsmeowClients[userID]; exists {
		cm.lastActivity[userID] = time.Now()
	}
	return cm.whatsmeowClients[userID]
}

func (cm *ClientManager) DeleteWhatsmeowClient(userID string) {
	cm.Lock()
	defer cm.Unlock()
	delete(cm.whatsmeowClients, userID)
	delete(cm.lastActivity, userID)
}

func (cm *ClientManager) SetHTTPClient(userID string, client *resty.Client) {
	cm.Lock()
	defer cm.Unlock()
	cm.httpClients[userID] = client
}

func (cm *ClientManager) GetHTTPClient(userID string) *resty.Client {
	cm.RLock()
	defer cm.RUnlock()
	return cm.httpClients[userID]
}

func (cm *ClientManager) DeleteHTTPClient(userID string) {
	cm.Lock()
	defer cm.Unlock()
	delete(cm.httpClients, userID)
}

func (cm *ClientManager) SetMyClient(userID string, client *MyClient) {
	cm.Lock()
	defer cm.Unlock()
	cm.myClients[userID] = client
}

func (cm *ClientManager) GetMyClient(userID string) *MyClient {
	cm.RLock()
	defer cm.RUnlock()
	return cm.myClients[userID]
}

func (cm *ClientManager) DeleteMyClient(userID string) {
	cm.Lock()
	defer cm.Unlock()
	delete(cm.myClients, userID)
}

// UpdateMyClientSubscriptions updates the event subscriptions of a client without reconnecting
func (cm *ClientManager) UpdateMyClientSubscriptions(userID string, subscriptions []string) {
	cm.Lock()
	defer cm.Unlock()
	if client, exists := cm.myClients[userID]; exists {
		client.subscriptions = subscriptions
		cm.lastActivity[userID] = time.Now()
	}
}

// UpdateActivity updates the last activity time for a session
func (cm *ClientManager) UpdateActivity(userID string) {
	cm.Lock()
	defer cm.Unlock()
	if _, exists := cm.whatsmeowClients[userID]; exists {
		cm.lastActivity[userID] = time.Now()
	}
}

// GetLastActivity returns the last activity time for a session
func (cm *ClientManager) GetLastActivity(userID string) (time.Time, bool) {
	cm.RLock()
	defer cm.RUnlock()
	lastAct, exists := cm.lastActivity[userID]
	return lastAct, exists
}

// UnloadInactiveSessions removes sessions from memory that have been inactive for more than the specified duration
// Sessions are still stored on disk and can be reloaded when needed
func (cm *ClientManager) UnloadInactiveSessions(inactiveDuration time.Duration) []string {
	cm.Lock()
	defer cm.Unlock()

	now := time.Now()
	var unloadedSessions []string

	for userID, lastAct := range cm.lastActivity {
		if now.Sub(lastAct) > inactiveDuration {
			// Check if client is connected - don't unload connected sessions
			if client, exists := cm.whatsmeowClients[userID]; exists && client != nil {
				if !client.IsConnected() {
					// Unload from memory but keep on disk
					delete(cm.whatsmeowClients, userID)
					delete(cm.httpClients, userID)
					delete(cm.myClients, userID)
					delete(cm.lastActivity, userID)
					unloadedSessions = append(unloadedSessions, userID)
				}
			} else {
				// Client doesn't exist, clean up activity tracking
				delete(cm.lastActivity, userID)
			}
		}
	}

	return unloadedSessions
}

// GetAllSessions returns a list of all active session IDs
func (cm *ClientManager) GetAllSessions() []string {
	cm.RLock()
	defer cm.RUnlock()

	sessions := make([]string, 0, len(cm.whatsmeowClients))
	for userID := range cm.whatsmeowClients {
		sessions = append(sessions, userID)
	}
	return sessions
}
