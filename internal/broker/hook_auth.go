package broker

import (
	"bytes"
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"strings"

	"github.com/google/uuid"
	"monstermq.io/edge/internal/auth"
	mqtt "monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/stores"
	storesqlite "monstermq.io/edge/internal/stores/sqlite"
)

// AuthHook bridges mochi's Auth/ACL callbacks to our Cache, with support for
// mutual TLS client certificate Common Name authentication.
type AuthHook struct {
	mqtt.HookBase
	cache                 *auth.Cache
	userStore             stores.UserStore
	useIdentityAsUsername bool
	autoCreateUser        bool
	logger                *slog.Logger
}

func NewAuthHook(
	cache *auth.Cache,
	userStore stores.UserStore,
	useIdentityAsUsername bool,
	autoCreateUser bool,
	logger *slog.Logger,
) *AuthHook {
	return &AuthHook{
		cache:                 cache,
		userStore:             userStore,
		useIdentityAsUsername: useIdentityAsUsername,
		autoCreateUser:        autoCreateUser,
		logger:                logger,
	}
}

func (h *AuthHook) ID() string { return "monstermq-auth" }

func (h *AuthHook) Provides(b byte) bool {
	return bytes.Contains([]byte{mqtt.OnConnectAuthenticate, mqtt.OnACLCheck}, []byte{b})
}

func extractPeerCertificateCommonName(c net.Conn) string {
	if c == nil {
		return ""
	}
	tlsConn, ok := c.(*tls.Conn)
	if !ok {
		return ""
	}
	state := tlsConn.ConnectionState()
	if !state.HandshakeComplete {
		if err := tlsConn.Handshake(); err != nil {
			return ""
		}
		state = tlsConn.ConnectionState()
	}
	if len(state.PeerCertificates) == 0 {
		return ""
	}
	return strings.TrimSpace(state.PeerCertificates[0].Subject.CommonName)
}

func (h *AuthHook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	if h.useIdentityAsUsername {
		certUsername := extractPeerCertificateCommonName(cl.Net.Conn)
		if certUsername != "" {
			existingUser, exists := h.cache.Lookup(certUsername)
			if exists {
				if !existingUser.Enabled {
					if h.logger != nil {
						h.logger.Warn("certificate user is disabled - rejecting connection", "client", cl.ID, "username", certUsername)
					}
					return false
				}
				cl.Properties.Username = []byte(certUsername)
				if h.logger != nil {
					h.logger.Info("authenticated via TLS client certificate", "client", cl.ID, "username", certUsername)
				}
				return true
			}

			if !h.autoCreateUser {
				if h.logger != nil {
					h.logger.Warn("no user account for certificate Common Name - rejecting connection", "client", cl.ID, "username", certUsername)
				}
				return false
			}

			if h.userStore == nil {
				if h.logger != nil {
					h.logger.Warn("cannot auto-create user: userStore is nil - rejecting connection", "client", cl.ID, "username", certUsername)
				}
				return false
			}

			hash, err := storesqlite.HashPassword(uuid.New().String())
			if err != nil {
				if h.logger != nil {
					h.logger.Warn("could not hash password for auto-created user", "client", cl.ID, "username", certUsername, "err", err)
				}
				return false
			}

			newUser := stores.User{
				Username:     certUsername,
				PasswordHash: hash,
				Enabled:      true,
				CanSubscribe: true,
				CanPublish:   true,
				IsAdmin:      false,
			}
			ctx := context.Background()
			if err := h.userStore.CreateUser(ctx, newUser); err != nil {
				_ = h.cache.Refresh(ctx)
				if u, ok := h.cache.Lookup(certUsername); ok && u.Enabled {
					cl.Properties.Username = []byte(certUsername)
					if h.logger != nil {
						h.logger.Info("authenticated via TLS client certificate", "client", cl.ID, "username", certUsername)
					}
					return true
				}
				if h.logger != nil {
					h.logger.Warn("could not create user from client certificate - rejecting connection", "client", cl.ID, "username", certUsername, "err", err)
				}
				return false
			}

			_ = h.cache.Refresh(ctx)
			cl.Properties.Username = []byte(certUsername)
			if h.logger != nil {
				h.logger.Info("created user from client certificate and authenticated", "client", cl.ID, "username", certUsername)
			}
			return true
		}
	}

	username := string(cl.Properties.Username)
	password := string(pk.Connect.Password)
	return h.cache.Validate(username, password)
}

func (h *AuthHook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	username := string(cl.Properties.Username)
	return h.cache.Allow(username, topic, write)
}
