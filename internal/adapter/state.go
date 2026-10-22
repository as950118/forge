package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// StateVersion is the sidecar state file format version.
const StateVersion = 1

// StateItem records one item Forge manages. It holds no secrets and no content:
// only what is needed to recognize the item again (kind, exact name, content hash,
// time of the last apply).
type StateItem struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Hash      string `json:"hash"`
	AppliedAt string `json:"applied_at"`
}

// State is the sidecar file (<repo>/.forge/sync-state.json, or
// ~/.forge/sync-state/<target>-user.json). Items that are not listed here were
// written by the user and are never modified or deleted by sync.
type State struct {
	Version int         `json:"version"`
	Target  string      `json:"target"`
	Scope   Scope       `json:"scope"`
	Items   []StateItem `json:"items"`
}

// NewState returns an empty state.
func NewState(target string, scope Scope) *State {
	return &State{Version: StateVersion, Target: target, Scope: scope, Items: []StateItem{}}
}

// ParseState reads a state file. A corrupt or foreign file is an error: guessing
// which items are managed could delete user data.
func ParseState(b []byte, target string, scope Scope) (*State, error) {
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, errors.New("상태 파일을 해석할 수 없습니다 (손상?). 관리 항목을 판별할 수 없어 중단합니다. 파일을 백업에서 복원하거나, 지운 뒤 다시 sync 하세요(그러면 기존 항목은 사용자 소유로 취급되어 충돌로 보고됩니다)")
	}
	if s.Version != StateVersion {
		return nil, fmt.Errorf("지원하지 않는 상태 파일 버전: %d (이 forge 는 %d)", s.Version, StateVersion)
	}
	if s.Target != target || s.Scope != scope {
		return nil, fmt.Errorf("상태 파일의 대상(%q/%q)이 이번 sync(%s/%s)와 다릅니다", cutStr(s.Target, 30), cutStr(string(s.Scope), 30), target, scope)
	}
	seen := map[string]bool{}
	for _, it := range s.Items {
		k := it.Kind + "\x00" + it.Name
		if seen[k] {
			return nil, errors.New("상태 파일에 중복 항목이 있습니다")
		}
		seen[k] = true
	}
	return &s, nil
}

// Find returns the item or nil.
func (s *State) Find(kind, name string) *StateItem {
	for i := range s.Items {
		if s.Items[i].Kind == kind && s.Items[i].Name == name {
			return &s.Items[i]
		}
	}
	return nil
}

// Upsert records an item; appliedAt is kept when the hash did not change.
func (s *State) Upsert(kind, name, hash, now string) {
	if it := s.Find(kind, name); it != nil {
		if it.Hash != hash {
			it.Hash, it.AppliedAt = hash, now
		}
		return
	}
	s.Items = append(s.Items, StateItem{Kind: kind, Name: name, Hash: hash, AppliedAt: now})
}

// Remove drops an item.
func (s *State) Remove(kind, name string) {
	out := s.Items[:0]
	for _, it := range s.Items {
		if !(it.Kind == kind && it.Name == name) {
			out = append(out, it)
		}
	}
	s.Items = out
}

// Marshal renders the state deterministically (sorted, 2-space indent, newline).
func (s *State) Marshal() []byte {
	c := *s
	c.Items = append([]StateItem{}, s.Items...)
	sort.Slice(c.Items, func(i, j int) bool {
		if c.Items[i].Kind != c.Items[j].Kind {
			return c.Items[i].Kind < c.Items[j].Kind
		}
		return c.Items[i].Name < c.Items[j].Name
	})
	b, _ := json.MarshalIndent(&c, "", "  ")
	return append(b, '\n')
}
