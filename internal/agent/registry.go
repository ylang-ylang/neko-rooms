package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

const (
	defaultTTLSeconds = 3600
	maxTTLSeconds     = 86400
)

var (
	errUnauthorized = errors.New("missing, invalid, expired or revoked temporary bearer key")
	errWrongRoom    = errors.New("temporary key belongs to another room")
	errShutdown     = errors.New("agent access manager is shut down")
)

// KeyMetadata deliberately contains neither the token nor its hash.
type KeyMetadata struct {
	KeyID             string    `json:"key_id"`
	RoomName          string    `json:"room_name"`
	Label             string    `json:"label"`
	CreatedAt         time.Time `json:"created_at"`
	ExpiresAt         time.Time `json:"expires_at"`
	ActiveConnections int       `json:"active_connections"`
}

type keyRecord struct {
	metadata KeyMetadata
	hash     [sha256.Size]byte
	timer    *time.Timer
	sessions map[*session]struct{}
}

// The registry lock linearizes authorization+registration, revocation and
// shutdown. A session is registered BEFORE lookup/dial/upgrade can start.
type registry struct {
	mu     sync.Mutex
	closed bool
	byHash map[[sha256.Size]byte]*keyRecord
	byID   map[string]*keyRecord
}

func newRegistry() *registry {
	return &registry{
		byHash: make(map[[sha256.Size]byte]*keyRecord),
		byID:   make(map[string]*keyRecord),
	}
}

func (r *registry) issue(room, label string, ttl int64) (KeyMetadata, string, error) {
	var random [48]byte
	if _, err := rand.Read(random[:]); err != nil {
		return KeyMetadata{}, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(random[:32])
	now := time.Now().UTC()
	record := &keyRecord{
		metadata: KeyMetadata{
			KeyID:     hex.EncodeToString(random[32:]),
			RoomName:  room,
			Label:     label,
			CreatedAt: now,
			ExpiresAt: now.Add(time.Duration(ttl) * time.Second),
		},
		hash:     sha256.Sum256([]byte(token)),
		sessions: make(map[*session]struct{}),
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return KeyMetadata{}, "", errShutdown
	}
	r.byHash[record.hash] = record
	r.byID[record.metadata.KeyID] = record
	record.timer = time.AfterFunc(time.Until(record.metadata.ExpiresAt), func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.byID[record.metadata.KeyID] == record {
			r.invalidateLocked(record)
		}
	})
	return record.metadata, token, nil
}

func (r *registry) acquire(ctx context.Context, token, room string) (*keyRecord, *session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record := r.byHash[sha256.Sum256([]byte(token))]
	if token == "" || record == nil || r.closed {
		return nil, nil, errUnauthorized
	}
	if !time.Now().Before(record.metadata.ExpiresAt) {
		r.invalidateLocked(record)
		return nil, nil, errUnauthorized
	}
	if record.metadata.RoomName != room {
		return nil, nil, errWrongRoom
	}
	ctx, cancel := context.WithDeadline(ctx, record.metadata.ExpiresAt)
	s := &session{ctx: ctx, cancel: cancel}
	record.sessions[s] = struct{}{}
	return record, s, nil
}

func (r *registry) release(record *keyRecord, s *session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s.close()
	delete(record.sessions, s)
}

func (r *registry) list(room string) []KeyMetadata {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make([]KeyMetadata, 0)
	for _, record := range r.byID {
		if !time.Now().Before(record.metadata.ExpiresAt) {
			r.invalidateLocked(record)
			continue
		}
		if record.metadata.RoomName != room {
			continue
		}
		metadata := record.metadata
		for s := range record.sessions {
			s.mu.Lock()
			if !s.closed && s.client != nil {
				metadata.ActiveConnections++
			}
			s.mu.Unlock()
		}
		keys = append(keys, metadata)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].CreatedAt.Before(keys[j].CreatedAt)
	})
	return keys
}

func (r *registry) revoke(room, keyID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	record := r.byID[keyID]
	if record == nil || record.metadata.RoomName != room {
		return false
	}
	r.invalidateLocked(record)
	return true
}

// Closing uses net.Conn.Close, not a WebSocket closing handshake: it cannot
// wait for an agent to cooperate. Keep the lock until both sockets are closed,
// so a concurrent DELETE/shutdown cannot return ahead of an earlier closer.
func (r *registry) invalidateLocked(record *keyRecord) {
	delete(r.byHash, record.hash)
	delete(r.byID, record.metadata.KeyID)
	record.timer.Stop()
	for s := range record.sessions {
		s.close()
		delete(record.sessions, s)
	}
}

func (r *registry) shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for _, record := range r.byID {
		r.invalidateLocked(record)
	}
}

type session struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	closed   bool
	upstream io.Closer
	client   io.Closer
}

func (s *session) attachUpstream(upstream io.Closer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		upstream.Close()
		return false
	}
	s.upstream = upstream
	return true
}

// Attach the raw client at Hijack, BEFORE the upgrader writes its handshake.
// Only the local ownership transfer shares the close lock, never network I/O:
// a stalled handshake cannot hold up expiry/revocation of established sockets.
type sessionResponseWriter struct {
	http.ResponseWriter
	session *session
}

func (w *sessionResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	s := w.session
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return nil, nil, errUnauthorized
	}
	client, buffered, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	s.client = client
	return client, buffered, nil
}

func (s *session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.cancel()
	if s.client != nil {
		s.client.Close()
	}
	if s.upstream != nil {
		s.upstream.Close()
	}
}
