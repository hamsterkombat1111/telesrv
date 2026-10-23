package rpc

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"telesrv/internal/domain"
)

const (
	loginTokenTTL        = 30 * time.Second
	loginTokenBytes      = 32
	loginTokenMaxRecords = 2048
)

type loginTokenTarget struct {
	rawAuthKeyID [8]byte
	authKeyID    [8]byte
	sessionID    int64
}

type loginTokenExport struct {
	token        []byte
	expires      time.Time
	accepted     bool
	acceptedAuth domain.Authorization
}

type loginTokenAcceptStart struct {
	target loginTokenTarget
	authz  domain.Authorization
}

type loginTokenRecord struct {
	token     []byte
	expires   time.Time
	target    loginTokenTarget
	authz     domain.Authorization
	exceptIDs map[int64]struct{}

	accepting      bool
	accepted       bool
	acceptedUserID int64
	acceptedAuth   domain.Authorization
	acceptedAt     time.Time
}

type loginTokenRegistry struct {
	mu       sync.Mutex
	byToken  map[string]*loginTokenRecord
	byTarget map[loginTokenTarget]*loginTokenRecord
}

func newLoginTokenRegistry() *loginTokenRegistry {
	return &loginTokenRegistry{
		byToken:  make(map[string]*loginTokenRecord),
		byTarget: make(map[loginTokenTarget]*loginTokenRecord),
	}
}

func (r *loginTokenRegistry) export(now time.Time, target loginTokenTarget, authz domain.Authorization, exceptIDs []int64) (loginTokenExport, error) {
	if r == nil {
		return loginTokenExport{}, fmt.Errorf("login token registry is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanupExpiredLocked(now)

	if rec := r.byTarget[target]; rec != nil && rec.expires.After(now) {
		if rec.accepted {
			return loginTokenExport{accepted: true, acceptedAuth: rec.acceptedAuth}, nil
		}
		return loginTokenExport{token: append([]byte(nil), rec.token...), expires: rec.expires}, nil
	}
	if len(r.byToken) >= loginTokenMaxRecords {
		r.evictOldestLocked()
	}

	token, err := randomLoginTokenLocked(r.byToken)
	if err != nil {
		return loginTokenExport{}, err
	}
	rec := &loginTokenRecord{
		token:     token,
		expires:   now.Add(loginTokenTTL),
		target:    target,
		authz:     authz,
		exceptIDs: loginTokenExceptSet(exceptIDs),
	}
	r.byToken[string(token)] = rec
	r.byTarget[target] = rec
	return loginTokenExport{token: append([]byte(nil), token...), expires: rec.expires}, nil
}

func (r *loginTokenRegistry) lookup(now time.Time, token []byte) (loginTokenExport, error) {
	if r == nil || len(token) == 0 {
		return loginTokenExport{}, authTokenInvalidErr()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.byToken[string(token)]
	if rec == nil {
		r.cleanupExpiredLocked(now)
		return loginTokenExport{}, authTokenInvalidErr()
	}
	if !rec.expires.After(now) {
		r.deleteLocked(rec)
		return loginTokenExport{}, authTokenExpiredErr()
	}
	r.cleanupExpiredLocked(now)
	if rec.accepted {
		return loginTokenExport{accepted: true, acceptedAuth: rec.acceptedAuth}, nil
	}
	return loginTokenExport{token: append([]byte(nil), rec.token...), expires: rec.expires}, nil
}

func (r *loginTokenRegistry) beginAccept(now time.Time, token []byte, userID int64) (loginTokenAcceptStart, error) {
	if r == nil || len(token) == 0 {
		return loginTokenAcceptStart{}, authTokenInvalidErr()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.byToken[string(token)]
	if rec == nil {
		r.cleanupExpiredLocked(now)
		return loginTokenAcceptStart{}, authTokenInvalidErr()
	}
	if !rec.expires.After(now) {
		r.deleteLocked(rec)
		return loginTokenAcceptStart{}, authTokenExpiredErr()
	}
	r.cleanupExpiredLocked(now)
	if rec.accepted || rec.accepting {
		return loginTokenAcceptStart{}, authTokenAlreadyAcceptedErr()
	}
	if _, denied := rec.exceptIDs[userID]; denied {
		return loginTokenAcceptStart{}, authTokenAlreadyAcceptedErr()
	}
	rec.accepting = true
	return loginTokenAcceptStart{target: rec.target, authz: rec.authz}, nil
}

func (r *loginTokenRegistry) finishAccept(now time.Time, token []byte, userID int64, acceptedAuth domain.Authorization) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.byToken[string(token)]
	if rec == nil {
		return
	}
	rec.accepting = false
	rec.accepted = true
	rec.acceptedUserID = userID
	rec.acceptedAuth = acceptedAuth
	rec.acceptedAt = now
	r.byTarget[rec.target] = rec
}

func (r *loginTokenRegistry) failAccept(token []byte) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.byToken[string(token)]; rec != nil {
		rec.accepting = false
	}
}

func (r *loginTokenRegistry) cleanupExpiredLocked(now time.Time) {
	for _, rec := range r.byToken {
		if !rec.expires.After(now) {
			r.deleteLocked(rec)
		}
	}
}

func (r *loginTokenRegistry) deleteLocked(rec *loginTokenRecord) {
	delete(r.byToken, string(rec.token))
	if r.byTarget[rec.target] == rec {
		delete(r.byTarget, rec.target)
	}
}

func (r *loginTokenRegistry) evictOldestLocked() {
	var oldest *loginTokenRecord
	for _, rec := range r.byToken {
		if oldest == nil || rec.expires.Before(oldest.expires) {
			oldest = rec
		}
	}
	if oldest != nil {
		r.deleteLocked(oldest)
	}
}

func randomLoginTokenLocked(existing map[string]*loginTokenRecord) ([]byte, error) {
	for i := 0; i < 4; i++ {
		token := make([]byte, loginTokenBytes)
		if _, err := rand.Read(token); err != nil {
			return nil, fmt.Errorf("generate login token: %w", err)
		}
		if existing[string(token)] == nil {
			return token, nil
		}
	}
	return nil, fmt.Errorf("generate login token: collision")
}

func loginTokenExceptSet(ids []int64) map[int64]struct{} {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id != 0 {
			out[id] = struct{}{}
		}
	}
	return out
}
