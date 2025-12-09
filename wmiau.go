package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/jmoiron/sqlx"
	"github.com/mdp/qrterminal/v3"
	"github.com/patrickmn/go-cache"
	"github.com/rs/zerolog/log"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"golang.org/x/net/proxy"
)

// db field declaration as *sqlx.DB
type MyClient struct {
	WAClient       *whatsmeow.Client
	eventHandlerID uint32
	userID         string
	token          string
	subscriptions  []string
	db             *sqlx.DB
	s              *server
}


func updateAndGetUserSubscriptions(mycli *MyClient) ([]string, error) {
	// Get updated events from cache/database
	currentEvents := ""
	userinfo2, found2 := userinfocache.Get(mycli.token)
	if found2 {
		currentEvents = userinfo2.(Values).Get("Events")
	} else {
		// If not in cache, get from database
		if err := mycli.db.Get(&currentEvents, "SELECT events FROM users WHERE id=$1", mycli.userID); err != nil {
			log.Warn().Err(err).Str("userID", mycli.userID).Msg("Could not get events from DB")
			return nil, err // Propagate the error
		}
	}

	// Update client subscriptions if changed
	eventarray := strings.Split(currentEvents, ",")
	var subscribedEvents []string
	if len(eventarray) == 1 && eventarray[0] == "" {
		subscribedEvents = []string{}
	} else {
		for _, arg := range eventarray {
			arg = strings.TrimSpace(arg)
			if arg != "" && Find(supportedEventTypes, arg) {
				subscribedEvents = append(subscribedEvents, arg)
			}
		}
	}

	// Update the client subscriptions
	mycli.subscriptions = subscribedEvents

	return subscribedEvents, nil
}

// In stdio mode, send as JSON-RPC notification
func sendEventStdio(mycli *MyClient, postmap map[string]interface{}) {
	if mycli.s != nil && mycli.s.mode == Stdio {
		eventType, ok := postmap["type"].(string)
		if ok {
			mycli.s.SendNotification(eventType, postmap)
		}
	}
}

// Connects to Whatsapp Websocket on server startup if last state was connected
func (s *server) connectOnStartup() {
	rows, err := s.db.Queryx("SELECT id,name,token,jid,webhook,events,proxy_url,CASE WHEN s3_enabled THEN 'true' ELSE 'false' END AS s3_enabled,media_delivery,COALESCE(history, 0) as history,hmac_key FROM users WHERE connected=1")
	if err != nil {
		log.Error().Err(err).Msg("DB Problem")
		return
	}
	defer rows.Close()
	for rows.Next() {
		txtid := ""
		token := ""
		jid := ""
		name := ""
		webhook := ""
		events := ""
		proxy_url := ""
		s3_enabled := ""
		media_delivery := ""
		var history int
		var hmac_key []byte
		err = rows.Scan(&txtid, &name, &token, &jid, &webhook, &events, &proxy_url, &s3_enabled, &media_delivery, &history, &hmac_key)
		if err != nil {
			log.Error().Err(err).Msg("DB Problem")
			return
		} else {
			hmacKeyEncrypted := ""
			if len(hmac_key) > 0 {
				hmacKeyEncrypted = base64.StdEncoding.EncodeToString(hmac_key)
			}

			log.Info().Str("token", token).Msg("Connect to Whatsapp on startup")
			v := Values{map[string]string{
				"Id":               txtid,
				"Name":             name,
				"Jid":              jid,
				"Webhook":          webhook,
				"Token":            token,
				"Proxy":            proxy_url,
				"Events":           events,
				"S3Enabled":        s3_enabled,
				"MediaDelivery":    media_delivery,
				"History":          fmt.Sprintf("%d", history),
				"HmacKeyEncrypted": hmacKeyEncrypted,
			}}
			userinfocache.Set(token, v, cache.NoExpiration)
			// Gets and set subscription to webhook events
			eventarray := strings.Split(events, ",")

			var subscribedEvents []string
			if len(eventarray) == 1 && eventarray[0] == "" {
				subscribedEvents = []string{}
			} else {
				for _, arg := range eventarray {
					if !Find(supportedEventTypes, arg) {
						log.Warn().Str("Type", arg).Msg("Event type discarded")
						continue
					}
					if !Find(subscribedEvents, arg) {
						subscribedEvents = append(subscribedEvents, arg)
					}
				}

			}
			eventstring := strings.Join(subscribedEvents, ",")
			log.Info().Str("events", eventstring).Str("jid", jid).Msg("Attempt to connect")
			killchannel[txtid] = make(chan bool)
			go s.startClient(txtid, jid, token, subscribedEvents)

			// Initialize S3 client if configured
			go func(userID string) {
				var s3Config struct {
					Enabled       bool   `db:"s3_enabled"`
					Endpoint      string `db:"s3_endpoint"`
					Region        string `db:"s3_region"`
					Bucket        string `db:"s3_bucket"`
					AccessKey     string `db:"s3_access_key"`
					SecretKey     string `db:"s3_secret_key"`
					PathStyle     bool   `db:"s3_path_style"`
					PublicURL     string `db:"s3_public_url"`
					RetentionDays int    `db:"s3_retention_days"`
				}

				err := s.db.Get(&s3Config, `
					SELECT s3_enabled, s3_endpoint, s3_region, s3_bucket, 
						   s3_access_key, s3_secret_key, s3_path_style, 
						   s3_public_url, s3_retention_days
					FROM users WHERE id = $1`, userID)

				if err != nil {
					log.Error().Err(err).Str("userID", userID).Msg("Failed to get S3 config")
					return
				}

				if s3Config.Enabled {
					// S3 initialization removed - GetS3Manager() not implemented
					// config := &S3Config{
					// 	Enabled:       s3Config.Enabled,
					// 	Endpoint:      s3Config.Endpoint,
					// 	Region:        s3Config.Region,
					// 	Bucket:        s3Config.Bucket,
					// 	AccessKey:     s3Config.AccessKey,
					// 	SecretKey:     s3Config.SecretKey,
					// 	PathStyle:     s3Config.PathStyle,
					// 	PublicURL:     s3Config.PublicURL,
					// 	RetentionDays: s3Config.RetentionDays,
					// }

					// err = GetS3Manager().InitializeS3Client(userID, config)
					// if err != nil {
					// 	log.Error().Err(err).Str("userID", userID).Msg("Failed to initialize S3 client on startup")
					// } else {
					// 	log.Info().Str("userID", userID).Msg("S3 client initialized on startup")
					// }
					log.Warn().Str("userID", userID).Msg("S3 enabled but initialization not implemented")
				}
			}(txtid)
		}
	}
	err = rows.Err()
	if err != nil {
		log.Error().Err(err).Msg("DB Problem")
	}
}

func parseJID(arg string) (types.JID, bool) {
	if arg[0] == '+' {
		arg = arg[1:]
	}
	if !strings.ContainsRune(arg, '@') {
		return types.NewJID(arg, types.DefaultUserServer), true
	} else {
		recipient, err := types.ParseJID(arg)
		if err != nil {
			log.Error().Err(err).Msg("Invalid JID")
			return recipient, false
		} else if recipient.User == "" {
			log.Error().Err(err).Msg("Invalid JID no server specified")
			return recipient, false
		}
		return recipient, true
	}
}

func (s *server) startClient(userID string, textjid string, token string, subscriptions []string) {
	log.Info().Str("userid", userID).Str("jid", textjid).Msg("Starting websocket connection to Whatsapp")

	// Connection retry constants
	const maxConnectionRetries = 3
	const connectionRetryBaseWait = 5 * time.Second

	var deviceStore *store.Device
	var err error

	// First handle the device store initialization
	if textjid != "" {
		jid, _ := parseJID(textjid)
		deviceStore, err = container.GetDevice(context.Background(), jid)
		if err != nil {
			log.Error().Err(err).Msg("Failed to get device")
			deviceStore = container.NewDevice()
		}
	} else {
		log.Warn().Msg("No jid found. Creating new device")
		deviceStore = container.NewDevice()
	}

	if deviceStore == nil {
		log.Warn().Msg("No store found. Creating new one")
		deviceStore = container.NewDevice()
	}

	clientLog := waLog.Stdout("Client", *waDebug, *colorOutput)

	// Create the client with initialized deviceStore
	var client *whatsmeow.Client
	if *waDebug != "" {
		client = whatsmeow.NewClient(deviceStore, clientLog)
	} else {
		client = whatsmeow.NewClient(deviceStore, nil)
	}

	// Now we can use the client with the manager
	clientManager.SetWhatsmeowClient(userID, client)

	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
	store.DeviceProps.Os = osName

	mycli := MyClient{client, 1, userID, token, subscriptions, s.db, s}
	mycli.eventHandlerID = mycli.WAClient.AddEventHandler(mycli.myEventHandler)

	// Store the MyClient in clientManager
	clientManager.SetMyClient(userID, &mycli)

	httpClient := resty.New()
	httpClient.SetRedirectPolicy(resty.FlexibleRedirectPolicy(15))
	if *waDebug == "DEBUG" {
		httpClient.SetDebug(true)
	}
	httpClient.SetTimeout(30 * time.Second)
	httpClient.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: true})
	httpClient.OnError(func(req *resty.Request, err error) {
		if v, ok := err.(*resty.ResponseError); ok {
			// v.Response contains the last response from the server
			// v.Err contains the original error
			log.Debug().Str("response", v.Response.String()).Msg("resty error")
			log.Error().Err(v.Err).Msg("resty error")
		}
	})

	// Set proxy if defined in DB (assumes users table contains proxy_url column)
	var proxyURL string
	err = s.db.Get(&proxyURL, "SELECT proxy_url FROM users WHERE id=$1", userID)
	if err == nil && proxyURL != "" {
		parsed, perr := url.Parse(proxyURL)
		if perr != nil {
			log.Warn().Err(perr).Str("proxy", proxyURL).Msg("Invalid proxy URL, skipping proxy setup")
		} else {

			log.Info().Str("proxy", proxyURL).Msg("Configuring proxy")

			if parsed.Scheme == "socks5" || parsed.Scheme == "socks5h" {
				dialer, derr := proxy.FromURL(parsed, nil)
				if derr != nil {
					log.Warn().Err(derr).Str("proxy", proxyURL).Msg("Failed to build SOCKS proxy dialer, skipping proxy setup")
				} else {
					httpClient.SetProxy(proxyURL)
					client.SetSOCKSProxy(dialer, whatsmeow.SetProxyOptions{})
					log.Info().Msg("SOCKS proxy configured successfully")
				}
			} else {
				httpClient.SetProxy(proxyURL)
				client.SetProxyAddress(parsed.String(), whatsmeow.SetProxyOptions{})
				log.Info().Msg("HTTP/HTTPS proxy configured successfully")
			}
		}
	}
	clientManager.SetHTTPClient(userID, httpClient)

	if client.Store.ID == nil {
		// No ID stored, new login
		qrChan, err := client.GetQRChannel(context.Background())
		if err != nil {
			// This error means that we're already logged in, so ignore it.
			if !errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
				log.Error().Err(err).Msg("Failed to get QR channel")
				return
			}
		} else {
			err = client.Connect() // Must connect to generate QR code
			if err != nil {
				log.Error().Err(err).Msg("Failed to connect client")
				return
			}

			myuserinfo, found := userinfocache.Get(token)

			for evt := range qrChan {
				if evt.Event == "code" {
					// Display QR code in terminal (useful for testing/developing)
					// Skip in stdio mode to avoid breaking JSON-RPC
					if *logType != "json" && s.mode != Stdio {
						qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
						fmt.Println("QR code:\n", evt.Code)
					}
					// Store encoded/embeded base64 QR on database for retrieval with the /qr endpoint
					image, _ := qrcode.Encode(evt.Code, qrcode.Medium, 256)
					base64qrcode := "data:image/png;base64," + base64.StdEncoding.EncodeToString(image)
					sqlStmt := `UPDATE users SET qrcode=$1 WHERE id=$2`
					_, err := s.db.Exec(sqlStmt, base64qrcode, userID)
					if err != nil {
						log.Error().Err(err).Msg(sqlStmt)
					} else {
						if found {
							v := updateUserInfo(myuserinfo, "Qrcode", base64qrcode)
							userinfocache.Set(token, v, cache.NoExpiration)
							log.Info().Str("qrcode", base64qrcode).Msg("update cache userinfo with qr code")
						}
					}

					//send QR code with webhook
					postmap := make(map[string]interface{})
					postmap["event"] = evt.Event
					postmap["qrCodeBase64"] = base64qrcode
					postmap["type"] = "QR"

					sendEventStdio(&mycli, postmap)

				} else if evt.Event == "timeout" {
					// Clear QR code from DB on timeout
					// Send webhook notifying QR timeout before cleanup
					postmap := make(map[string]interface{})
					postmap["event"] = evt.Event
					postmap["type"] = "QRTimeout"
					sendEventStdio(&mycli, postmap)

					sqlStmt := `UPDATE users SET qrcode='' WHERE id=$1`
					_, err := s.db.Exec(sqlStmt, userID)
					if err != nil {
						log.Error().Err(err).Msg(sqlStmt)
					} else {
						if found {
							v := updateUserInfo(myuserinfo, "Qrcode", "")
							userinfocache.Set(token, v, cache.NoExpiration)
						}
					}
					log.Warn().Msg("QR timeout killing channel")
					clientManager.DeleteWhatsmeowClient(userID)
					clientManager.DeleteMyClient(userID)
					clientManager.DeleteHTTPClient(userID)
					killchannel[userID] <- true
				} else if evt.Event == "success" {
					log.Info().Msg("QR pairing ok!")
					// Clear QR code after pairing
					sqlStmt := `UPDATE users SET qrcode='', connected=1 WHERE id=$1`
					_, err := s.db.Exec(sqlStmt, userID)
					if err != nil {
						log.Error().Err(err).Msg(sqlStmt)
					} else {
						if found {
							v := updateUserInfo(myuserinfo, "Qrcode", "")
							userinfocache.Set(token, v, cache.NoExpiration)
						}
					}
				} else {
					log.Info().Str("event", evt.Event).Msg("Login event")
				}
			}
		}

	} else {
		// Already logged in, just connect
		log.Info().Msg("Already logged in, just connect")

		// Retry logic with linear backoff
		var lastErr error

		for attempt := 0; attempt < maxConnectionRetries; attempt++ {
			if attempt > 0 {
				waitTime := time.Duration(attempt) * connectionRetryBaseWait
				log.Warn().
					Int("attempt", attempt+1).
					Int("max_retries", maxConnectionRetries).
					Dur("wait_time", waitTime).
					Msg("Retrying connection after delay")
				time.Sleep(waitTime)
			}

			err = client.Connect()
			if err == nil {
				log.Info().
					Int("attempt", attempt+1).
					Msg("Successfully connected to WhatsApp")
				break
			}

			lastErr = err
			log.Warn().
				Err(err).
				Int("attempt", attempt+1).
				Int("max_retries", maxConnectionRetries).
				Msg("Failed to connect to WhatsApp")
		}

		if lastErr != nil {
			log.Error().
				Err(lastErr).
				Str("userid", userID).
				Int("attempts", maxConnectionRetries).
				Msg("Failed to connect to WhatsApp after all retry attempts")

			clientManager.DeleteWhatsmeowClient(userID)
			clientManager.DeleteMyClient(userID)
			clientManager.DeleteHTTPClient(userID)

			sqlStmt := `UPDATE users SET qrcode='', connected=0 WHERE id=$1`
			_, dbErr := s.db.Exec(sqlStmt, userID)
			if dbErr != nil {
				log.Error().Err(dbErr).Msg("Failed to update user status after connection error")
			}

			// Use the existing mycli instance from outer scope
			postmap := make(map[string]interface{})
			postmap["event"] = "ConnectFailure"
			postmap["error"] = lastErr.Error()
			postmap["type"] = "ConnectFailure"
			postmap["attempts"] = maxConnectionRetries
			postmap["reason"] = "Failed to connect after retry attempts"
			sendEventStdio(&mycli, postmap)

			return
		}
	}

	// Keep connected client live until disconnected/killed
	for {
		select {
		case <-killchannel[userID]:
			log.Info().Str("userid", userID).Msg("Received kill signal")
			client.Disconnect()
			clientManager.DeleteWhatsmeowClient(userID)
			clientManager.DeleteMyClient(userID)
			clientManager.DeleteHTTPClient(userID)
			sqlStmt := `UPDATE users SET qrcode='', connected=0 WHERE id=$1`
			_, err := s.db.Exec(sqlStmt, userID)
			if err != nil {
				log.Error().Err(err).Msg(sqlStmt)
			}
			return
		default:
			time.Sleep(1000 * time.Millisecond)
			//log.Info().Str("jid",textjid).Msg("Loop the loop")
		}
	}
}

func fileToBase64(filepath string) (string, string, error) {
	data, err := os.ReadFile(filepath)
	if err != nil {
		return "", "", err
	}
	mimeType := http.DetectContentType(data)
	return base64.StdEncoding.EncodeToString(data), mimeType, nil
}

func (mycli *MyClient) myEventHandler(rawEvt interface{}) {
	txtid := mycli.userID
	postmap := make(map[string]interface{})
	postmap["event"] = rawEvt

	switch evt := rawEvt.(type) {
	case *events.AppStateSyncComplete:
		if len(mycli.WAClient.Store.PushName) > 0 && evt.Name == appstate.WAPatchCriticalBlock {
			err := mycli.WAClient.SendPresence(context.Background(), types.PresenceAvailable)
			if err != nil {
				log.Warn().Err(err).Msg("Failed to send available presence")
			} else {
				log.Info().Msg("Marked self as available")
			}
		}
	case *events.Connected, *events.PushNameSetting:
		postmap["type"] = "Connected"
		if len(mycli.WAClient.Store.PushName) == 0 {
			break
		}
		// Send presence available when connecting and when the pushname is changed.
		// This makes sure that outgoing messages always have the right pushname.
		err := mycli.WAClient.SendPresence(context.Background(), types.PresenceAvailable)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to send available presence")
		} else {
			log.Info().Msg("Marked self as available")
		}
		sqlStmt := `UPDATE users SET connected=1 WHERE id=$1`
		_, err = mycli.db.Exec(sqlStmt, mycli.userID)
		if err != nil {
			log.Error().Err(err).Msg(sqlStmt)
			return
		}
	case *events.PairSuccess:
		log.Info().Str("userid", mycli.userID).Str("token", mycli.token).Str("ID", evt.ID.String()).Str("BusinessName", evt.BusinessName).Str("Platform", evt.Platform).Msg("QR Pair Success")
		jid := evt.ID
		sqlStmt := `UPDATE users SET jid=$1 WHERE id=$2`
		_, err := mycli.db.Exec(sqlStmt, jid, mycli.userID)
		if err != nil {
			log.Error().Err(err).Msg(sqlStmt)
			return
		}

		postmap["type"] = "PairSuccess"

		myuserinfo, found := userinfocache.Get(mycli.token)
		if !found {
			log.Warn().Msg("No user info cached on pairing?")
		} else {
			txtid = myuserinfo.(Values).Get("Id")
			token := myuserinfo.(Values).Get("Token")
			v := updateUserInfo(myuserinfo, "Jid", fmt.Sprintf("%s", jid))
			userinfocache.Set(token, v, cache.NoExpiration)
			log.Info().Str("jid", jid.String()).Str("userid", txtid).Str("token", token).Msg("User information set")
		}

	case *events.StreamReplaced:
		log.Info().Msg("Received StreamReplaced event")
		return
	case *events.Message:
		// Message events removed - only sending functionality is supported
		return
	case *events.Receipt:
		// Receipt events removed - only sending functionality is supported
		return
	case *events.Presence:
		// Presence events removed - only sending functionality is supported
		return
	case *events.HistorySync:
		// HistorySync events removed - only sending functionality is supported
		return
	case *events.AppState:
		log.Info().Str("index", fmt.Sprintf("%+v", evt.Index)).Str("actionValue", fmt.Sprintf("%+v", evt.SyncActionValue)).Msg("App state event received")
	case *events.LoggedOut:
		postmap["type"] = "LoggedOut"
		log.Info().Str("reason", evt.Reason.String()).Msg("Logged out")
		defer func() {
			// Use a non-blocking send to prevent a deadlock if the receiver has already terminated.
			select {
			case killchannel[mycli.userID] <- true:
			default:
			}
		}()
		sqlStmt := `UPDATE users SET connected=0 WHERE id=$1`
		_, err := mycli.db.Exec(sqlStmt, mycli.userID)
		if err != nil {
			log.Error().Err(err).Msg(sqlStmt)
		}
	case *events.ChatPresence:
		postmap["type"] = "ChatPresence"
		log.Info().Str("state", fmt.Sprintf("%s", evt.State)).Str("media", fmt.Sprintf("%s", evt.Media)).Str("chat", evt.MessageSource.Chat.String()).Str("sender", evt.MessageSource.Sender.String()).Msg("Chat Presence received")
	case *events.CallOffer:
		postmap["type"] = "CallOffer"
		log.Info().Str("event", fmt.Sprintf("%+v", evt)).Msg("Got call offer")
	case *events.CallAccept:
		postmap["type"] = "CallAccept"
		log.Info().Str("event", fmt.Sprintf("%+v", evt)).Msg("Got call accept")
	case *events.CallTerminate:
		postmap["type"] = "CallTerminate"
		log.Info().Str("event", fmt.Sprintf("%+v", evt)).Msg("Got call terminate")
	case *events.CallOfferNotice:
		postmap["type"] = "CallOfferNotice"
		log.Info().Str("event", fmt.Sprintf("%+v", evt)).Msg("Got call offer notice")
	case *events.CallRelayLatency:
		postmap["type"] = "CallRelayLatency"
		log.Info().Str("event", fmt.Sprintf("%+v", evt)).Msg("Got call relay latency")
	case *events.Disconnected:
		postmap["type"] = "Disconnected"
		log.Info().Str("reason", fmt.Sprintf("%+v", evt)).Msg("Disconnected from Whatsapp")
	case *events.ConnectFailure:
		postmap["type"] = "ConnectFailure"
		log.Error().Str("reason", fmt.Sprintf("%+v", evt)).Msg("Failed to connect to Whatsapp")
	case *events.UndecryptableMessage:
		postmap["type"] = "UndecryptableMessage"
		log.Warn().Str("info", evt.Info.SourceString()).Msg("Undecryptable message received")
	case *events.MediaRetry:
		postmap["type"] = "MediaRetry"
		log.Info().Str("messageID", evt.MessageID).Msg("Media retry event")
	case *events.GroupInfo:
		postmap["type"] = "GroupInfo"
		log.Info().Str("jid", evt.JID.String()).Msg("Group info updated")
	case *events.JoinedGroup:
		postmap["type"] = "JoinedGroup"
		log.Info().Str("jid", evt.JID.String()).Msg("Joined group")
	case *events.Picture:
		postmap["type"] = "Picture"
		log.Info().Str("jid", evt.JID.String()).Msg("Picture updated")
	case *events.BlocklistChange:
		postmap["type"] = "BlocklistChange"
		log.Info().Str("jid", evt.JID.String()).Msg("Blocklist changed")
	case *events.Blocklist:
		postmap["type"] = "Blocklist"
		log.Info().Msg("Blocklist received")
	case *events.KeepAliveRestored:
		postmap["type"] = "KeepAliveRestored"
		log.Info().Msg("Keep alive restored")
	case *events.KeepAliveTimeout:
		postmap["type"] = "KeepAliveTimeout"
		log.Warn().Msg("Keep alive timeout")
	case *events.ClientOutdated:
		postmap["type"] = "ClientOutdated"
		log.Warn().Msg("Client outdated")
	case *events.TemporaryBan:
		postmap["type"] = "TemporaryBan"
		log.Info().Msg("Temporary ban")
	case *events.StreamError:
		postmap["type"] = "StreamError"
		log.Error().Str("code", evt.Code).Msg("Stream error")
	case *events.PairError:
		postmap["type"] = "PairError"
		log.Error().Msg("Pair error")
	case *events.PrivacySettings:
		postmap["type"] = "PrivacySettings"
		log.Info().Msg("Privacy settings updated")
	case *events.UserAbout:
		postmap["type"] = "UserAbout"
		log.Info().Str("jid", evt.JID.String()).Msg("User about updated")
	case *events.OfflineSyncCompleted:
		postmap["type"] = "OfflineSyncCompleted"
		log.Info().Msg("Offline sync completed")
	case *events.OfflineSyncPreview:
		postmap["type"] = "OfflineSyncPreview"
		log.Info().Msg("Offline sync preview")
	case *events.IdentityChange:
		postmap["type"] = "IdentityChange"
		log.Info().Str("jid", evt.JID.String()).Msg("Identity changed")
	case *events.NewsletterJoin:
		postmap["type"] = "NewsletterJoin"
		log.Info().Str("jid", evt.ID.String()).Msg("Newsletter joined")
	case *events.NewsletterLeave:
		postmap["type"] = "NewsletterLeave"
		log.Info().Str("jid", evt.ID.String()).Msg("Newsletter left")
	case *events.NewsletterMuteChange:
		postmap["type"] = "NewsletterMuteChange"
		log.Info().Str("jid", evt.ID.String()).Msg("Newsletter mute changed")
	case *events.NewsletterLiveUpdate:
		postmap["type"] = "NewsletterLiveUpdate"
		log.Info().Msg("Newsletter live update")
	case *events.FBMessage:
		postmap["type"] = "FBMessage"
		log.Info().Str("info", evt.Info.SourceString()).Msg("Facebook message received")
	default:
		log.Warn().Str("event", fmt.Sprintf("%+v", evt)).Msg("Unhandled event")
	}

	// Webhooks removed - only stdio mode notifications
	sendEventStdio(mycli, postmap)
}
