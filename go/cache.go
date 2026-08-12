package main

import "sync"

// users と owners の行は INSERT しか行われず、一度作られたら変化しない。
// 認証は全リクエストに乗るため、アクセストークンからの引き当てをメモリに持つ。
// 走行のたびに init.sh が DB を作り直すので、initialize で必ず捨てる。
type authCache[T any] struct {
	mu      sync.RWMutex
	entries map[string]*T
}

func newAuthCache[T any]() *authCache[T] {
	return &authCache[T]{entries: map[string]*T{}}
}

func (c *authCache[T]) get(token string) (*T, bool) {
	c.mu.RLock()
	v, ok := c.entries[token]
	c.mu.RUnlock()
	return v, ok
}

func (c *authCache[T]) set(token string, v *T) {
	c.mu.Lock()
	c.entries[token] = v
	c.mu.Unlock()
}

func (c *authCache[T]) reset() {
	c.mu.Lock()
	c.entries = map[string]*T{}
	c.mu.Unlock()
}

// chairs は座標やカウンタが走行中に変わり続けるため、行ごとキャッシュしてはいけない。
// 変化しないカラムだけを別の型に切り出して、可変な値を取り違えられないようにする。
type chairIdentity struct {
	ID      string `db:"id"`
	OwnerID string `db:"owner_id"`
	Name    string `db:"name"`
	Model   string `db:"model"`
}

var (
	userCache  = newAuthCache[User]()
	ownerCache = newAuthCache[Owner]()
	chairCache = newAuthCache[chairIdentity]()
)

func resetCaches() {
	userCache.reset()
	ownerCache.reset()
	chairCache.reset()
}
