package emby

import (
	"encoding/json"
	"os"
	"sync"
)

// userItemState 是桥接侧保存的、Emby 有而飞牛没有的用户状态：
//   - Likes：喜欢/不喜欢（Emby 详情页的 👍/👎）
//   - Hidden：从"继续观看"移除（不改变飞牛进度）
//
// 飞牛没有这两个概念，所以必须落本地；按"用户名 + 条目 guid"隔离。
type userItemState struct {
	Likes  *bool `json:"likes,omitempty"`
	Hidden bool  `json:"hidden,omitempty"`
}

type userStateStore struct {
	mu   sync.Mutex
	path string
	data map[string]map[string]userItemState
}

// newUserStateStore 加载本地状态文件（不存在/损坏时从空开始，不致命）。
func newUserStateStore(path string) *userStateStore {
	s := &userStateStore{path: path, data: map[string]map[string]userItemState{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.data)
	}
	if s.data == nil {
		s.data = map[string]map[string]userItemState{}
	}
	return s
}

func userStateKey(user string) string {
	if user == "" {
		return "default"
	}
	return user
}

// Like 返回 (是否喜欢, 是否已设置)。
func (s *userStateStore) Like(user, guid string) (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.data[userStateKey(user)][guid]
	if !ok || st.Likes == nil {
		return false, false
	}
	return *st.Likes, true
}

// SetLike 保存喜欢/不喜欢状态。
func (s *userStateStore) SetLike(user, guid string, like bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := userStateKey(user)
	if s.data[u] == nil {
		s.data[u] = map[string]userItemState{}
	}
	st := s.data[u][guid]
	st.Likes = &like
	s.data[u][guid] = st
	s.saveLocked()
}

// ClearLike 清除喜欢/不喜欢状态（DELETE Rating）。
func (s *userStateStore) ClearLike(user, guid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := userStateKey(user)
	st, ok := s.data[u][guid]
	if !ok {
		return
	}
	st.Likes = nil
	if !st.Hidden {
		delete(s.data[u], guid)
	} else {
		s.data[u][guid] = st
	}
	s.saveLocked()
}

// Hidden 判断条目是否被用户从"继续观看"隐藏。
func (s *userStateStore) Hidden(user, guid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[userStateKey(user)][guid].Hidden
}

// SetHidden 设置/取消"从继续观看隐藏"。
func (s *userStateStore) SetHidden(user, guid string, hidden bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := userStateKey(user)
	if s.data[u] == nil {
		s.data[u] = map[string]userItemState{}
	}
	st := s.data[u][guid]
	st.Hidden = hidden
	if !hidden && st.Likes == nil {
		delete(s.data[u], guid)
		s.saveLocked()
		return
	}
	s.data[u][guid] = st
	s.saveLocked()
}

func (s *userStateStore) saveLocked() {
	b, err := json.Marshal(s.data)
	if err != nil {
		return
	}
	_ = os.WriteFile(s.path, b, 0o600)
}
