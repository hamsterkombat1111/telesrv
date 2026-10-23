package memory

import (
	"context"
	"sync"

	"telesrv/internal/domain"
)

type privacyStoreKey struct {
	ownerUserID int64
	key         domain.PrivacyKey
}

// PrivacyStore is an in-memory account privacy rule store for tests/dev mode.
type PrivacyStore struct {
	mu    sync.RWMutex
	rules map[privacyStoreKey]domain.PrivacyRules
}

func NewPrivacyStore() *PrivacyStore {
	return &PrivacyStore{rules: make(map[privacyStoreKey]domain.PrivacyRules)}
}

func (s *PrivacyStore) GetPrivacyRules(_ context.Context, ownerUserID int64, key domain.PrivacyKey) (domain.PrivacyRules, bool, error) {
	s.mu.RLock()
	rules, ok := s.rules[privacyStoreKey{ownerUserID: ownerUserID, key: key}]
	s.mu.RUnlock()
	return clonePrivacyRules(rules), ok, nil
}

func (s *PrivacyStore) SetPrivacyRules(_ context.Context, rules domain.PrivacyRules) error {
	s.mu.Lock()
	s.rules[privacyStoreKey{ownerUserID: rules.OwnerUserID, key: rules.Key}] = clonePrivacyRules(rules)
	s.mu.Unlock()
	return nil
}

func (s *PrivacyStore) ListPrivacyRules(_ context.Context, ownerUserIDs []int64, keys []domain.PrivacyKey) ([]domain.PrivacyRules, error) {
	if len(ownerUserIDs) == 0 || len(keys) == 0 {
		return nil, nil
	}
	owners := make(map[int64]struct{}, len(ownerUserIDs))
	for _, id := range ownerUserIDs {
		owners[id] = struct{}{}
	}
	keySet := make(map[domain.PrivacyKey]struct{}, len(keys))
	for _, key := range keys {
		keySet[key] = struct{}{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]domain.PrivacyRules, 0, len(s.rules))
	for k, rules := range s.rules {
		if _, ok := owners[k.ownerUserID]; !ok {
			continue
		}
		if _, ok := keySet[k.key]; !ok {
			continue
		}
		out = append(out, clonePrivacyRules(rules))
	}
	return out, nil
}

func clonePrivacyRules(in domain.PrivacyRules) domain.PrivacyRules {
	out := in
	out.Rules = make([]domain.PrivacyRule, len(in.Rules))
	for i, rule := range in.Rules {
		out.Rules[i] = rule
		out.Rules[i].UserIDs = append([]int64(nil), rule.UserIDs...)
		out.Rules[i].ChatIDs = append([]int64(nil), rule.ChatIDs...)
	}
	return out
}
