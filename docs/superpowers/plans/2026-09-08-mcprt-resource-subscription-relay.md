# mcprt: resources/subscribe 中継 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** downstreamの`resources/subscribe`/`resources/unsubscribe`を、該当リソースを提供するbackendへ参照カウント方式で中継し、backendからの`notifications/resources/updated`を購読中の全downstreamセッションへリレーする。backend再接続時は自動再購読、downstreamセッション切断時は自動クリーンアップする。

**Architecture:** `internal/gateway`に新規`SubscriptionRegistry`（URIごとの参照カウント・backend/originalURI対応表を持つ、backend I/Oを一切行わない独立コンポーネント）を追加し、`gateway.Relays`の3番目のフィールドとして配線する。`Server`に`SubscribeHandler`/`UnsubscribeHandler`（`s.relays.Subscriptions != nil`のときだけ設定）を追加し、`resourceTable`から解決したbackend/元URIを使ってレジストリを操作、参照カウントが0→1のときだけ実際に`b.Session.Subscribe`する。downstreamセッションの切断検知はgo-sdkに公開APIがないため、`superviseBackend`が既に`Session.Wait()`をbackend切断検知に使っているのと同じ手法を、購読済みdownstream `*mcp.ServerSession`ごとに1つのgoroutineとして使う。backend再接続時の再購読は`internal/cli/server.go`の`superviseBackend`が`gw.ConnectBackend`呼び出し直後に行う。

設計書は`SubscriptionRegistry.Relay`を「購読者ごとに`ServerSession.NotifyResourceUpdated`相当を呼ぶ」実装として示しているが、**go-sdk v1.7.0に`*mcp.ServerSession`側の該当メソッドは存在しない**（`NotifyProgress`はあるが、リソース更新の単一セッション向け通知メソッドはない）。一方`*mcp.Server`自身が`resources/subscribe`成功時に購読セッションを内部管理しており（`resourceSubscriptions map[string]map[*ServerSession]jsonrpc.ID`、非公開）、`(*mcp.Server).ResourceUpdated(ctx, params)`を呼べばプロトコルバージョンを考慮した正しいfan-out を自動的に行う（10秒の内部タイムアウト付き）。本プランでは`Relay`を、レジストリ自身が持つ「backendName一致確認」ガード（`ProgressRegistry.Relay`のbackend不一致ガードと同じ役割）だけ行い、実際の配送は`mcpSrv.ResourceUpdated`に委譲する設計に変更する。この変更により`subscription`構造体の`sessions`フィールドは参照カウント（`Subscribe`/`Unsubscribe`/`SessionClosed`）専用になり、`Relay`はそれを一切参照しない。

設計書の`superviseBackend`再購読コードスニペットは、ループ変数名`c`が外側の`connectResult`変数`c`と衝突し、`c.backend`という存在しないフィールア参照になっている（`subscriptionToClose`は`BackendName`/`OriginalURI`のみ持つ）。本プランではループ変数を`sub`にリネームし、外側の`c.backend`（今まさに接続し直したbackend接続）を使う。

**Tech Stack:** Go 1.x, `github.com/modelcontextprotocol/go-sdk v1.7.0`（`mcp`パッケージ）, 標準の`go test`（`-race`込み）。

**Spec:** `docs/superpowers/specs/2026-09-07-mcprt-resource-subscription-relay-design.md`

## Global Constraints

- 対象は`resourceTable`に登録された具体リソースのURIのみ。`resourceTemplateTable`（リソーステンプレート由来の動的URI）は対象外 -- `completionHandler`と違い、`resourceTemplateTable`を一切参照しない。
- 購読状態は永続化しない（プロセス再起動・SIGHUP再読み込みで購読は失われる。既存のmcprtの全状態がインメモリであることと一貫する）。
- `SubscriptionRegistry`はbackend I/Oを一切持たない：`Subscribe`/`Unsubscribe`/`SessionClosed`/`BackendReconnected`は`needUpstream`/`needUpstreamUnsubscribe`のbool、または再購読/再解除が必要なURIのリストを返すだけで、実際の`b.Session.Subscribe`/`Unsubscribe`は呼び出し元（`subscribeHandler`/`unsubscribeHandler`/`startSessionCloseWatcherOnce`/`superviseBackend`）が行う。`Relay`だけは例外的にdownstream側I/O（`mcpSrv.ResourceUpdated`経由）を行う -- `ProgressRegistry.Relay`が`entry.session.NotifyProgress`を直接呼ぶのと同じ前例に従う。
- ロギング：再購読失敗（`BackendReconnected`後の`Subscribe`エラー）だけ`logger.Warn`。それ以外（正常な`Subscribe`/`Unsubscribe`/`Relay`、downstreamセッション切断によるクリーンアップの`Unsubscribe`失敗）はログしない -- 高頻度・副作用なしの経路のため。ただし`subscribeHandler`/`unsubscribeHandler`自身のエラー（未登録URI・backend未検出・backend呼び出し失敗）は、`completionHandler`が確立した「エラー時は`s.logger.Warn`してから返す」という本コードベースの既存の慣習に従う。
- `mcp.ServerOptions.SubscribeHandler`と`UnsubscribeHandler`は両方同時にセットしないと`mcp.NewServer`がpanicする（片方だけの設定は不可）。両方を同じ条件（`cfg.Relays.Subscriptions != nil`）でのみ設定すること。
- `go.mod`のモジュールパスは`github.com/wtnb75/mcprt`。SDKのパッケージ名は`mcp`（import path `github.com/modelcontextprotocol/go-sdk/mcp`、`v1.7.0`）。

---

## ファイル構成

| ファイル | 種別 | 責務 |
|---|---|---|
| `internal/gateway/subscription.go` | 新規 | `SubscriptionRegistry`: URIごとの参照カウント・backend対応表、`Subscribe`/`Unsubscribe`/`SessionClosed`/`BackendReconnected`/`Relay` |
| `internal/gateway/subscription_test.go` | 新規 | `SubscriptionRegistry`の単体テスト |
| `internal/backend/backend.go` | 変更 | `ChangeCallbacks.OnResourceUpdated`フィールド追加、`connectOnce`内で配線、`degradedCB`にも引き継ぐ |
| `internal/backend/backend_test.go` | 変更 | `OnResourceUpdated`が発火することを確認するテストを追加 |
| `internal/gateway/gateway.go` | 変更 | `Relays.Subscriptions`フィールド追加、`Server.watchedSessions`フィールド追加、`subscribeHandler`/`unsubscribeHandler`/`startSessionCloseWatcherOnce`を追加、`New`で条件付き配線 |
| `internal/gateway/gateway_test.go` | 変更 | 購読の参照カウント・リレー・エラー・切断クリーンアップの統合テストを追加 |
| `internal/cli/server.go` | 変更 | `buildGateway`で`SubscriptionRegistry`を構築、`superviseBackend`で`OnResourceUpdated`配線と再接続後の再購読ロジックを追加 |
| `internal/cli/server_test.go` | 変更 | e2eテスト2件（通常の更新リレー、backend再接続後も購読が生き続ける）を追加 |
| `README.md` | 変更 | `resources/subscribe`/`notifications/resources/updated`が中継されるようになったことを記述 |

---

## Task 1: SubscriptionRegistry コンポーネント

**Files:**
- Create: `internal/gateway/subscription.go`
- Create: `internal/gateway/subscription_test.go`

**Interfaces:**
- Produces: `gateway.NewSubscriptionRegistry() *SubscriptionRegistry`、`(*SubscriptionRegistry).Subscribe(session *mcp.ServerSession, uri, backendName, originalURI string) (needUpstream bool)`、`(*SubscriptionRegistry).Unsubscribe(session *mcp.ServerSession, uri string) (needUpstreamUnsubscribe bool)`、`(*SubscriptionRegistry).SessionClosed(session *mcp.ServerSession) []subscriptionToClose`（`subscriptionToClose{BackendName, OriginalURI string}`、非公開型だがフィールドはエクスポートされている）、`(*SubscriptionRegistry).BackendReconnected(backendName string) []subscriptionToClose`、`(*SubscriptionRegistry).Relay(ctx context.Context, mcpSrv *mcp.Server, logger *slog.Logger, backendName, originalURI string)`。以降のタスクはこれらのシグネチャをそのまま使う。

- [ ] **Step 1: `internal/gateway/subscription.go`を作成する**

```go
package gateway

import (
	"context"
	"log/slog"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SubscriptionRegistry tracks, per resource URI, which downstream
// ServerSessions currently want notifications/resources/updated for it --
// and, implicitly, whether mcprt itself currently has an active upstream
// subscription with the owning backend (reference-counted: mcprt
// subscribes upstream once, on the first downstream subscriber, and
// unsubscribes once the last one leaves). Subscribe/Unsubscribe/
// SessionClosed/BackendReconnected never touch a *backend.Backend
// directly -- their bool/slice return values tell the caller
// (gateway.go's subscribeHandler/unsubscribeHandler/
// startSessionCloseWatcherOnce, and internal/cli's superviseBackend)
// exactly when an upstream Subscribe/Unsubscribe call is needed, keeping
// this type free of backend I/O and easy to test in isolation -- the same
// design CallRouter and ProgressRegistry already use. Relay is the one
// exception: it does perform I/O, but only to the downstream side (via
// *mcp.Server, not *backend.Backend), matching ProgressRegistry.Relay's
// own precedent of calling the downstream side directly rather than
// routing back through a caller.
type SubscriptionRegistry struct {
	mu   sync.Mutex
	subs map[string]*subscription // keyed by URI -- exposed and original are the same string for resources (prefix is never applied to resource URIs), so one key serves both purposes
}

// subscription is one URI's current subscribers plus the backend that owns
// it. backendName/originalURI are set once, when the URI's first
// subscriber arrives (Subscribe), and never change while any subscriber
// remains -- they describe which backend mcprt's own upstream Subscribe
// call went to, so a later Unsubscribe/Relay/BackendReconnected can
// address the same backend without re-resolving resourceTable.
type subscription struct {
	backendName string
	originalURI string
	sessions    map[*mcp.ServerSession]bool
}

// subscriptionToClose is one URI whose last downstream subscriber just
// left (SessionClosed) or that needs re-subscribing after a backend
// reconnect (BackendReconnected) -- both callers need exactly
// backendName/originalURI to issue the matching upstream
// Subscribe/Unsubscribe call.
type subscriptionToClose struct {
	BackendName string
	OriginalURI string
}

// NewSubscriptionRegistry returns an empty registry, ready to use.
func NewSubscriptionRegistry() *SubscriptionRegistry {
	return &SubscriptionRegistry{subs: make(map[string]*subscription)}
}

// Subscribe records session's interest in uri (backendName/originalURI
// describe the owning backend, resolved the same way resourceReadHandler
// resolves them). Calling Subscribe again for a session/uri pair that's
// already recorded is a no-op (idempotent on the map, not double-counted).
// Reports needUpstream=true exactly when this is uri's first subscriber --
// the caller must then issue the backend's actual resources/subscribe
// itself; SubscriptionRegistry has no *backend.Backend to call it with.
func (r *SubscriptionRegistry) Subscribe(session *mcp.ServerSession, uri, backendName, originalURI string) (needUpstream bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	sub, ok := r.subs[uri]
	if !ok {
		sub = &subscription{backendName: backendName, originalURI: originalURI, sessions: make(map[*mcp.ServerSession]bool)}
		r.subs[uri] = sub
	}
	needUpstream = len(sub.sessions) == 0
	sub.sessions[session] = true
	return needUpstream
}

// Unsubscribe removes session's interest in uri. Reports
// needUpstreamUnsubscribe=true exactly when session was uri's last
// subscriber -- the caller must then issue the backend's actual
// resources/unsubscribe itself. Unsubscribing a uri/session pair that was
// never subscribed (or already removed) is a no-op, reporting false.
func (r *SubscriptionRegistry) Unsubscribe(session *mcp.ServerSession, uri string) (needUpstreamUnsubscribe bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	sub, ok := r.subs[uri]
	if !ok {
		return false
	}
	delete(sub.sessions, session)
	if len(sub.sessions) == 0 {
		delete(r.subs, uri)
		return true
	}
	return false
}

// SessionClosed removes every subscription session held, across every URI
// -- called once, when session's Wait() returns (see gateway.go's
// startSessionCloseWatcherOnce), to clean up a downstream client that
// disconnected without ever calling resources/unsubscribe. Reports the
// URIs that lost their last subscriber as a result, each needing an
// upstream Unsubscribe.
func (r *SubscriptionRegistry) SessionClosed(session *mcp.ServerSession) []subscriptionToClose {
	r.mu.Lock()
	defer r.mu.Unlock()

	var closed []subscriptionToClose
	for uri, sub := range r.subs {
		if !sub.sessions[session] {
			continue
		}
		delete(sub.sessions, session)
		if len(sub.sessions) == 0 {
			closed = append(closed, subscriptionToClose{BackendName: sub.backendName, OriginalURI: sub.originalURI})
			delete(r.subs, uri)
		}
	}
	return closed
}

// BackendReconnected reports every URI currently subscribed against
// backendName, regardless of how many downstream sessions still want it --
// for the reconnect path (see superviseBackend's wiring in
// internal/cli/server.go) to re-issue Subscribe on the backend's fresh
// *backend.Backend. An existing subscription must survive a backend
// disconnect/reconnect cycle exactly like tools/resources/prompts
// list-changed handlers already do; a fresh backend connection has no
// memory of subscriptions the previous connection held.
func (r *SubscriptionRegistry) BackendReconnected(backendName string) []subscriptionToClose {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []subscriptionToClose
	for _, sub := range r.subs {
		if sub.backendName == backendName {
			out = append(out, subscriptionToClose{BackendName: sub.backendName, OriginalURI: sub.originalURI})
		}
	}
	return out
}

// Relay notifies mcpSrv's downstream subscribers that originalURI on
// backendName changed, but only if originalURI is currently a tracked
// upstream subscription owned by backendName -- guarding against a
// notification for a URI mcprt never subscribed to, or (like
// ProgressRegistry.Relay's backend-mismatch guard) one a DIFFERENT
// backend is sending under a URI string that collides with another
// backend's own subscription.
//
// Unlike ProgressRegistry.Relay (which owns a direct *mcp.ServerSession
// per entry and calls NotifyProgress on it), Relay does not iterate this
// registry's own session set to deliver the notification: go-sdk's
// *mcp.ServerSession has no exported per-resource notify method (unlike
// NotifyProgress), and go-sdk's *mcp.Server already tracks, internally,
// per URI, exactly which downstream sessions called resources/subscribe
// for it (populated as a side effect of subscribeHandler/
// unsubscribeHandler succeeding) -- including protocol-version-aware
// delivery mcprt has no way to replicate from outside the SDK. Relay
// therefore delegates the actual fan-out to mcpSrv.ResourceUpdated, which
// already does this correctly; re-implementing it here would either
// duplicate that internal bookkeeping or silently drop the
// protocol-version handling go-sdk's own delivery path relies on.
func (r *SubscriptionRegistry) Relay(ctx context.Context, mcpSrv *mcp.Server, logger *slog.Logger, backendName, originalURI string) {
	r.mu.Lock()
	sub, ok := r.subs[originalURI]
	r.mu.Unlock()
	if !ok || sub.backendName != backendName {
		return
	}

	if err := mcpSrv.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: originalURI}); err != nil {
		logger.Warn("resource update relay failed", "uri", originalURI, "backend", backendName, "error", err)
	}
}
```

- [ ] **Step 2: `internal/gateway/subscription_test.go`を作成する**

```go
package gateway_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/gateway"
)

func TestSubscriptionRegistry_SubscribeFirstSubscriberNeedsUpstream(t *testing.T) {
	r := gateway.NewSubscriptionRegistry()
	sessionA := &mcp.ServerSession{}
	sessionB := &mcp.ServerSession{}

	if need := r.Subscribe(sessionA, "file:///a", "backend-a", "file:///a"); !need {
		t.Fatal("first Subscribe for a URI: needUpstream = false, want true")
	}
	if need := r.Subscribe(sessionB, "file:///a", "backend-a", "file:///a"); need {
		t.Fatal("second Subscribe for the same URI: needUpstream = true, want false")
	}
	// Same session subscribing again to the same URI is idempotent, not a
	// third distinct subscriber.
	if need := r.Subscribe(sessionA, "file:///a", "backend-a", "file:///a"); need {
		t.Fatal("re-Subscribe by an already-subscribed session: needUpstream = true, want false")
	}
}

func TestSubscriptionRegistry_UnsubscribeLastSubscriberNeedsUpstreamUnsubscribe(t *testing.T) {
	r := gateway.NewSubscriptionRegistry()
	sessionA := &mcp.ServerSession{}
	sessionB := &mcp.ServerSession{}
	r.Subscribe(sessionA, "file:///a", "backend-a", "file:///a")
	r.Subscribe(sessionB, "file:///a", "backend-a", "file:///a")

	if need := r.Unsubscribe(sessionA, "file:///a"); need {
		t.Fatal("Unsubscribe with another subscriber remaining: needUpstreamUnsubscribe = true, want false")
	}
	if need := r.Unsubscribe(sessionB, "file:///a"); !need {
		t.Fatal("Unsubscribe of the last subscriber: needUpstreamUnsubscribe = false, want true")
	}
	// Unsubscribing again (already gone) is a no-op, not an error/panic.
	if need := r.Unsubscribe(sessionB, "file:///a"); need {
		t.Fatal("Unsubscribe of an already-removed session: needUpstreamUnsubscribe = true, want false")
	}
	// A URI nobody ever subscribed to is also a no-op.
	if need := r.Unsubscribe(sessionA, "file:///never"); need {
		t.Fatal("Unsubscribe of an unknown URI: needUpstreamUnsubscribe = true, want false")
	}
}

func TestSubscriptionRegistry_SessionClosedReturnsURIsThatLostLastSubscriber(t *testing.T) {
	r := gateway.NewSubscriptionRegistry()
	sessionA := &mcp.ServerSession{}
	sessionB := &mcp.ServerSession{}
	r.Subscribe(sessionA, "file:///a", "backend-a", "file:///a")
	r.Subscribe(sessionA, "file:///b", "backend-b", "file:///b")
	r.Subscribe(sessionB, "file:///a", "backend-a", "file:///a") // sessionA is not the last subscriber of file:///a

	closed := r.SessionClosed(sessionA)
	if len(closed) != 1 {
		t.Fatalf("SessionClosed(sessionA) returned %d entries, want 1 (only file:///b lost its last subscriber)", len(closed))
	}
	if closed[0].OriginalURI != "file:///b" || closed[0].BackendName != "backend-b" {
		t.Fatalf("SessionClosed(sessionA)[0] = %+v, want {BackendName: backend-b, OriginalURI: file:///b}", closed[0])
	}

	// sessionA already fully removed: a second call must not report anything
	// again.
	if closed := r.SessionClosed(sessionA); len(closed) != 0 {
		t.Fatalf("second SessionClosed(sessionA) = %+v, want empty (already cleaned up)", closed)
	}

	closedB := r.SessionClosed(sessionB)
	if len(closedB) != 1 || closedB[0].OriginalURI != "file:///a" || closedB[0].BackendName != "backend-a" {
		t.Fatalf("SessionClosed(sessionB) = %+v, want [{backend-a file:///a}] (sessionB was file:///a's last remaining subscriber)", closedB)
	}
}

func TestSubscriptionRegistry_BackendReconnectedReturnsOnlyThatBackendsURIs(t *testing.T) {
	r := gateway.NewSubscriptionRegistry()
	sessionA := &mcp.ServerSession{}
	r.Subscribe(sessionA, "file:///a", "backend-a", "file:///a")
	r.Subscribe(sessionA, "file:///b", "backend-b", "file:///b")
	r.Subscribe(sessionA, "file:///a2", "backend-a", "file:///a2")

	got := r.BackendReconnected("backend-a")
	if len(got) != 2 {
		t.Fatalf("BackendReconnected(backend-a) returned %d entries, want 2", len(got))
	}
	var uris []string
	for _, c := range got {
		if c.BackendName != "backend-a" {
			t.Fatalf("BackendReconnected(backend-a) entry %+v has BackendName != backend-a", c)
		}
		uris = append(uris, c.OriginalURI)
	}
	sort.Strings(uris)
	if !slices.Equal(uris, []string{"file:///a", "file:///a2"}) {
		t.Fatalf("BackendReconnected(backend-a) URIs = %v, want [file:///a file:///a2]", uris)
	}

	if got := r.BackendReconnected("backend-c"); len(got) != 0 {
		t.Fatalf("BackendReconnected(backend-c) = %+v, want empty (no subscriptions for that backend)", got)
	}
}

// TestSubscriptionRegistry_ConcurrentSubscribeUnsubscribeSessionClosed
// exercises Subscribe/Unsubscribe/SessionClosed/BackendReconnected from
// many goroutines at once -- go test -race must find nothing.
func TestSubscriptionRegistry_ConcurrentSubscribeUnsubscribeSessionClosed(t *testing.T) {
	r := gateway.NewSubscriptionRegistry()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session := &mcp.ServerSession{}
			uri := fmt.Sprintf("file:///%d", i%5)
			r.Subscribe(session, uri, "backend-a", uri)
			r.Unsubscribe(session, uri)
			r.SessionClosed(session)
			r.BackendReconnected("backend-a")
		}(i)
	}
	wg.Wait()

	// The registry must still be usable afterward.
	session := &mcp.ServerSession{}
	if need := r.Subscribe(session, "file:///final", "backend-a", "file:///final"); !need {
		t.Fatal("Subscribe after concurrent stress: needUpstream = false, want true (registry should be empty by now)")
	}
}

// newSubscribableServer returns a bare *mcp.Server with SubscribeHandler/
// UnsubscribeHandler wired to always succeed (this exercises
// SubscriptionRegistry.Relay in isolation, not gateway.Server's own
// resourceTable-based validation -- see gateway_test.go for that), plus a
// connected client session (for the test to call Subscribe on directly)
// and a channel that receives every notifications/resources/updated URI
// the client's ResourceUpdatedHandler sees.
func newSubscribableServer(t *testing.T) (srv *mcp.Server, clientSession *mcp.ClientSession, resourceUpdatedCh <-chan string, cleanup func()) {
	t.Helper()

	srv = mcp.NewServer(&mcp.Implementation{Name: "probe", Version: "v1"}, &mcp.ServerOptions{
		SubscribeHandler:   func(context.Context, *mcp.SubscribeRequest) error { return nil },
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
	})
	httpSrv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))

	ch := make(chan string, 128)
	client := mcp.NewClient(&mcp.Implementation{Name: "probe-client", Version: "v1"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			ch <- req.Params.URI
		},
	})
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: httpSrv.URL}, nil)
	if err != nil {
		t.Fatalf("connect probe client: %v", err)
	}

	return srv, cs, ch, func() {
		_ = cs.Close()
		httpSrv.Close()
	}
}

func TestSubscriptionRegistry_RelayNotifiesSubscribedDownstream(t *testing.T) {
	srv, clientSession, resourceUpdatedCh, cleanup := newSubscribableServer(t)
	defer cleanup()
	ctx := context.Background()

	if err := clientSession.Subscribe(ctx, &mcp.SubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("client Subscribe: %v", err)
	}

	r := gateway.NewSubscriptionRegistry()
	r.Subscribe(&mcp.ServerSession{}, "file:///a", "backend-a", "file:///a")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r.Relay(ctx, srv, logger, "backend-a", "file:///a")

	select {
	case uri := <-resourceUpdatedCh:
		if uri != "file:///a" {
			t.Fatalf("resource updated URI = %q, want file:///a", uri)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("downstream ResourceUpdatedHandler did not fire within 5s of Relay")
	}
}

func TestSubscriptionRegistry_RelayIgnoresUnknownURI(t *testing.T) {
	srv, clientSession, resourceUpdatedCh, cleanup := newSubscribableServer(t)
	defer cleanup()
	ctx := context.Background()
	if err := clientSession.Subscribe(ctx, &mcp.SubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("client Subscribe: %v", err)
	}

	r := gateway.NewSubscriptionRegistry()
	// No r.Subscribe call at all: the registry never learned about
	// file:///a, even though the SDK-side client did subscribe to it.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r.Relay(ctx, srv, logger, "backend-a", "file:///a")

	select {
	case uri := <-resourceUpdatedCh:
		t.Fatalf("received resource update for URI %q, want none relayed (registry never tracked it)", uri)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestSubscriptionRegistry_RelayIgnoresBackendMismatch proves that a
// notification claiming a URI the registry has on record as owned by a
// DIFFERENT backend is silently dropped -- the counterpart of
// TestProgressRegistry_RelayDropsBackendMismatch for this registry.
func TestSubscriptionRegistry_RelayIgnoresBackendMismatch(t *testing.T) {
	srv, clientSession, resourceUpdatedCh, cleanup := newSubscribableServer(t)
	defer cleanup()
	ctx := context.Background()
	if err := clientSession.Subscribe(ctx, &mcp.SubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("client Subscribe: %v", err)
	}

	r := gateway.NewSubscriptionRegistry()
	r.Subscribe(&mcp.ServerSession{}, "file:///a", "backend-a", "file:///a")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r.Relay(ctx, srv, logger, "backend-b", "file:///a")

	select {
	case uri := <-resourceUpdatedCh:
		t.Fatalf("received resource update for URI %q from mismatched backend, want none relayed", uri)
	case <-time.After(200 * time.Millisecond):
	}
}
```

- [ ] **Step 3: テストを実行して確認する**

Run: `go test ./internal/gateway/... -run TestSubscriptionRegistry -v`
Expected: すべてPASS。

- [ ] **Step 4: `-race`で並行テストを確認する**

Run: `go test ./internal/gateway/... -run TestSubscriptionRegistry_ConcurrentSubscribeUnsubscribeSessionClosed -race -v`
Expected: PASS、race検出なし。

- [ ] **Step 5: パッケージ全体のビルド・vetを確認する**

Run: `go build ./... && go vet ./... && gofmt -l internal/gateway/subscription.go internal/gateway/subscription_test.go`
Expected: すべて成功、`gofmt -l`は何も出力しない。

- [ ] **Step 6: コミット**

```bash
git add internal/gateway/subscription.go internal/gateway/subscription_test.go
git commit -m "feat(gateway): add SubscriptionRegistry for resources/subscribe relay"
```

---

## Task 2: `backend.ChangeCallbacks.OnResourceUpdated` の配線

**Files:**
- Modify: `internal/backend/backend.go:59-75`（`ChangeCallbacks`構造体）, `:103-118`（`Connect`）, `:158-177`（`connectOnce`）
- Test: `internal/backend/backend_test.go`

**Interfaces:**
- Consumes: なし（Task 1とは独立）。
- Produces: `backend.ChangeCallbacks.OnResourceUpdated func(context.Context, *mcp.ResourceUpdatedNotificationRequest)`。Task 4がこれを`superviseBackend`から使う。

- [ ] **Step 1: `ChangeCallbacks`に`OnResourceUpdated`フィールドを追加する**

`internal/backend/backend.go`の`ChangeCallbacks`構造体（現在74行目の`OnElicit`フィールドの直後）に追加する：

```go
	// OnResourceUpdated, if non-nil, is wired as the backend-facing
	// mcp.Client's ResourceUpdatedHandler -- fires when the backend sends
	// notifications/resources/updated for a URI mcprt has subscribed to on
	// some downstream session's behalf (see gateway.SubscriptionRegistry).
	// Like OnProgress, this is a plain notification callback (no result to
	// return).
	OnResourceUpdated func(context.Context, *mcp.ResourceUpdatedNotificationRequest)
```

- [ ] **Step 2: `connectOnce`内で配線する**

`internal/backend/backend.go`の`connectOnce`関数、現在175-177行目の

```go
	if cb.OnElicit != nil {
		clientOpts.ElicitationHandler = cb.OnElicit
	}
```

の直後に追加する：

```go
	if cb.OnResourceUpdated != nil {
		clientOpts.ResourceUpdatedHandler = cb.OnResourceUpdated
	}
```

- [ ] **Step 3: `Connect`の劣化リトライ（`degradedCB`）にも引き継ぐ**

`internal/backend/backend.go`の`Connect`関数、現在110行目の

```go
			degradedCB := ChangeCallbacks{OnProgress: cb.OnProgress, OnElicit: cb.OnElicit}
```

を、以下に変更する（`ResourceUpdatedHandler`は`hasListChangedHandlers`が調べる3つのlist_changedハンドラ -- `subscriptions/listen`のプローブ失敗の原因になり得るもの -- とは無関係な、`OnProgress`/`OnElicit`と同種の“ペイロード付き通知コールバック”なので、劣化後の再接続でも同じように引き継ぐ必要がある。引き継がないと、`subscriptions/listen`未対応で劣化した backend では通知だけがサイレントに届かなくなる）：

```go
			degradedCB := ChangeCallbacks{OnProgress: cb.OnProgress, OnElicit: cb.OnElicit, OnResourceUpdated: cb.OnResourceUpdated}
```

- [ ] **Step 4: 失敗するテストを書く**

`internal/backend/backend_test.go`の`TestConnect_ElicitationCallback`（555-595行目）の直後に追加する：

```go
// TestConnect_ResourceUpdatedCallback checks that ChangeCallbacks.
// OnResourceUpdated fires with the backend's notifications/resources/
// updated payload when the backend sends one for a URI mcprt subscribed
// to.
func TestConnect_ResourceUpdatedCallback(t *testing.T) {
	fakeServer := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v1"}, &mcp.ServerOptions{
		SubscribeHandler:   func(context.Context, *mcp.SubscribeRequest) error { return nil },
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
	})
	fakeServer.AddResource(&mcp.Resource{URI: "file:///a", Name: "a"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "content"}}}, nil
		})

	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fakeServer }, nil))
	defer srv.Close()

	ctx := context.Background()
	received := make(chan *mcp.ResourceUpdatedNotificationParams, 1)
	b, err := backend.Connect(ctx, config.BackendConfig{Name: "fake", Transport: "http", URL: srv.URL},
		backend.ChangeCallbacks{OnResourceUpdated: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			received <- req.Params
		}})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = b.Close() }()

	if err := b.Session.Subscribe(ctx, &mcp.SubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := fakeServer.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: "file:///a"}); err != nil {
		t.Fatalf("backend ResourceUpdated: %v", err)
	}

	select {
	case p := <-received:
		if p.URI != "file:///a" {
			t.Fatalf("OnResourceUpdated params = %+v, want URI=file:///a", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnResourceUpdated did not fire within 5s of the backend sending an update")
	}
}
```

- [ ] **Step 5: テストを実行してパスすることを確認する**

Run: `go test ./internal/backend/... -run TestConnect_ResourceUpdatedCallback -v`
Expected: PASS。

- [ ] **Step 6: パッケージ全体のテストとビルドを確認する**

Run: `go test ./internal/backend/... -v 2>&1 | tail -60 && go build ./... && go vet ./... && gofmt -l internal/backend/backend.go internal/backend/backend_test.go`
Expected: 全PASS、無回帰、`gofmt -l`は何も出力しない。

- [ ] **Step 7: コミット**

```bash
git add internal/backend/backend.go internal/backend/backend_test.go
git commit -m "feat(backend): wire notifications/resources/updated to ChangeCallbacks.OnResourceUpdated"
```

---

## Task 3: `gateway`パッケージへの統合（`SubscribeHandler`/`UnsubscribeHandler`/`New`）

**Files:**
- Modify: `internal/gateway/gateway.go:53-60`（`Relays`）, `:62-94`（`NewConfig`のドキュメントコメント）, `:96-133`（`Server`構造体）, `:183-236`（`New`）
- Test: `internal/gateway/gateway_test.go`

**Interfaces:**
- Consumes: Task 1の`SubscriptionRegistry`一式、Task 2の`backend.ChangeCallbacks.OnResourceUpdated`（統合テストで使う）。
- Produces: `gateway.Relays.Subscriptions *SubscriptionRegistry`フィールド、`(*Server).MCP()`は既存（`Relay`呼び出しでこれを使う）。Task 4がこれらをそのまま使う。

- [ ] **Step 1: `Relays`と`NewConfig`のドキュメントを更新する**

`internal/gateway/gateway.go`の現在53-60行目：

```go
// Relays bundles the optional cross-call correlation services a gateway
// can wire in. A nil field means that feature is disabled, matching the
// existing nil-means-disabled convention each of *ProgressRegistry and
// *CallRouter already had as standalone parameters.
type Relays struct {
	Progress *ProgressRegistry
	Calls    *CallRouter
}
```

を、以下に変更する：

```go
// Relays bundles the optional cross-call correlation services a gateway
// can wire in. A nil field means that feature is disabled, matching the
// existing nil-means-disabled convention each of *ProgressRegistry,
// *CallRouter, and *SubscriptionRegistry already had as standalone
// parameters.
type Relays struct {
	Progress      *ProgressRegistry
	Calls         *CallRouter
	Subscriptions *SubscriptionRegistry
}
```

現在66行目の

```go
// nil Relays.Progress/Relays.Calls means that relay feature is disabled.
```

を

```go
// nil Relays.Progress/Relays.Calls/Relays.Subscriptions means that relay
// feature is disabled.
```

に変更する。

- [ ] **Step 2: `Server`構造体に`watchedSessions`フィールドを追加する**

現在110-133行目の`Server`構造体、`relays   Relays`の直後（117行目、`mu sync.Mutex`の直前）に追加する：

```go
	// watchedSessions records which downstream *mcp.ServerSession already
	// has a startSessionCloseWatcherOnce goroutine running for it, so a
	// session that calls resources/subscribe more than once doesn't spawn
	// a second, redundant Wait()-then-cleanup goroutine for the same
	// session. Empty and unused whenever relays.Subscriptions is nil (the
	// subscription feature is disabled). Guarded by mu, same as every
	// other Server field below.
	watchedSessions map[*mcp.ServerSession]bool
```

（このフィールドは`mu sync.Mutex`の直前、`backends`/`maskKeys`/`relays`と同じブロックに置く。）

- [ ] **Step 3: `New`を条件付き配線に変更する**

現在183-236行目の`New`関数のうち、`Server{...}`構造体リテラル（190-204行目）と`mcpSrv := mcp.NewServer(...)`（206-212行目）を、以下に置き換える：

```go
	s := &Server{
		logger:   cfg.Logger,
		backends: cfg.Backends,
		maskKeys: cfg.MaskKeys,
		relays:   cfg.Relays,

		watchedSessions: make(map[*mcp.ServerSession]bool),

		toolEntries:   cfg.Entries.Tools,
		toolTable:     emptyTable(cfg.Tables.Tools),
		toolOverrides: cfg.Overrides.Tools,

		resourceEntries:           cfg.Entries.Resources,
		resourceTable:             emptyTable(cfg.Tables.Resources),
		resourceOverrides:         cfg.Overrides.Resources,
		resourceTemplateEntries:   cfg.Entries.ResourceTemplates,
		resourceTemplateTable:     emptyTable(cfg.Tables.ResourceTemplates),
		resourceTemplateOverrides: cfg.Overrides.ResourceTemplates,

		promptEntries:   cfg.Entries.Prompts,
		promptTable:     emptyTable(cfg.Tables.Prompts),
		promptOverrides: cfg.Overrides.Prompts,
	}

	opts := &mcp.ServerOptions{
		Logger:                    cfg.Logger,
		KeepAlive:                 cfg.KeepAlive,
		KeepAliveFailureThreshold: cfg.KeepAliveFailureThreshold,
		CompletionHandler:         s.completionHandler,
	}
	if cfg.Relays.Subscriptions != nil {
		opts.SubscribeHandler = s.subscribeHandler
		opts.UnsubscribeHandler = s.unsubscribeHandler
	}
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "mcprt", Version: "v1"}, opts)
	s.mcp = mcpSrv
```

（残りの`if cfg.Tables.Tools != nil { ... }`以降の登録ループはそのまま変更しない。）

- [ ] **Step 4: `subscribeHandler`/`unsubscribeHandler`/`startSessionCloseWatcherOnce`を追加する**

`internal/gateway/gateway.go`の`completionHandler`関数の直後（現在288行目、`registerTool`の直前）に追加する：

```go
// subscribeHandler forwards resources/subscribe to the backend that owns
// req.Params.URI, resolved through the same resourceTable registerResource
// already populates at registration time (unlike completionHandler,
// resourceTemplateTable is deliberately not consulted here -- subscribing
// to a resource template's dynamically-generated URIs is out of scope, see
// this plan's spec). It records the downstream session's interest in
// relays.Subscriptions and issues the backend's actual resources/subscribe
// only when this is the URI's first subscriber (reference-counted).
func (s *Server) subscribeHandler(ctx context.Context, req *mcp.SubscribeRequest) error {
	s.mu.Lock()
	resolved, ok := s.resourceTable.Items[req.Params.URI]
	if !ok {
		s.mu.Unlock()
		s.logger.Warn("subscribe: unknown resource", "uri", req.Params.URI)
		return fmt.Errorf("subscribe: unknown resource %q", req.Params.URI)
	}
	b := s.backends[resolved.BackendName]
	s.mu.Unlock()

	if b == nil {
		s.logger.Warn("subscribe: backend not found", "backend", resolved.BackendName, "uri", req.Params.URI)
		return fmt.Errorf("subscribe: backend %q not found for resource %q", resolved.BackendName, req.Params.URI)
	}

	s.startSessionCloseWatcherOnce(req.Session)

	needUpstream := s.relays.Subscriptions.Subscribe(req.Session, req.Params.URI, resolved.BackendName, resolved.OriginalName)
	if needUpstream {
		if err := b.Session.Subscribe(ctx, &mcp.SubscribeParams{URI: resolved.OriginalName}); err != nil {
			s.logger.Warn("subscribe: backend call failed", "backend", b.Name, "uri", req.Params.URI, "error", err)
			return err
		}
	}
	return nil
}

// unsubscribeHandler forwards resources/unsubscribe symmetrically to
// subscribeHandler: it resolves req.Params.URI through resourceTable the
// same way, removes the downstream session's interest from
// relays.Subscriptions, and issues the backend's actual
// resources/unsubscribe only when this was the URI's last subscriber.
//
// Because it re-resolves resourceTable (rather than trusting whatever
// backend/originalURI relays.Subscriptions itself recorded at Subscribe
// time), a resource a backend has since removed via list_changed -- while
// a downstream session is still subscribed to it -- makes
// unsubscribeHandler fail with "unknown resource" even though
// relays.Subscriptions still holds a live entry for it; that entry is
// then only cleared later, by SessionClosed, when the session eventually
// disconnects. Handling that gap (e.g. by having relays.Subscriptions
// itself remember backendName/originalURI and having Unsubscribe return
// them, so this lookup wouldn't depend on resourceTable staying accurate)
// is a deliberate simplification matching the design spec's own scope,
// not attempted here.
func (s *Server) unsubscribeHandler(ctx context.Context, req *mcp.UnsubscribeRequest) error {
	s.mu.Lock()
	resolved, ok := s.resourceTable.Items[req.Params.URI]
	if !ok {
		s.mu.Unlock()
		s.logger.Warn("unsubscribe: unknown resource", "uri", req.Params.URI)
		return fmt.Errorf("unsubscribe: unknown resource %q", req.Params.URI)
	}
	b := s.backends[resolved.BackendName]
	s.mu.Unlock()

	needUpstreamUnsubscribe := s.relays.Subscriptions.Unsubscribe(req.Session, req.Params.URI)
	if needUpstreamUnsubscribe && b != nil {
		if err := b.Session.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: resolved.OriginalName}); err != nil {
			s.logger.Warn("unsubscribe: backend call failed", "backend", b.Name, "uri", req.Params.URI, "error", err)
			return err
		}
	}
	return nil
}

// startSessionCloseWatcherOnce spawns, at most once per session, a
// goroutine that waits for session to disconnect (session.Wait(), the same
// technique superviseBackend already uses for backend-side disconnect
// detection -- go-sdk has no public downstream-session-close hook) and then
// cleans up every subscription that session held, unsubscribing upstream
// for any URI it was the last subscriber of. Called from subscribeHandler
// on every subscribe, but only the first call for a given session actually
// spawns the goroutine (guarded by s.watchedSessions) -- a session that
// subscribes to several URIs must not get one watcher goroutine per URI.
func (s *Server) startSessionCloseWatcherOnce(session *mcp.ServerSession) {
	s.mu.Lock()
	if s.watchedSessions[session] {
		s.mu.Unlock()
		return
	}
	s.watchedSessions[session] = true
	s.mu.Unlock()

	go func() {
		_ = session.Wait()
		toClose := s.relays.Subscriptions.SessionClosed(session)
		for _, c := range toClose {
			if b := s.Backend(c.BackendName); b != nil {
				_ = b.Session.Unsubscribe(context.Background(), &mcp.UnsubscribeParams{URI: c.OriginalURI})
			}
		}
		s.mu.Lock()
		delete(s.watchedSessions, session)
		s.mu.Unlock()
	}()
}
```

- [ ] **Step 5: `go build`でコンパイルエラーを洗い出し、機械的に直す**

Run: `go build ./...`
Expected: エラーなくビルドできる（既存の`gateway.New`呼び出しはすべて`Relays`フィールドを省略可能なゼロ値のまま使っているため、シグネチャ変更は不要で、コンパイルエラーは出ないはず。もし出た場合は該当箇所を確認して直す）。

- [ ] **Step 6: 統合テストを`gateway_test.go`に追加する**

`internal/gateway/gateway_test.go`の末尾に追加する（ファイル冒頭の`import`に`sync/atomic`が無ければ追加する）：

```go
// subscribableFakeBackend bundles a fake backend *mcp.Server that supports
// resources/subscribe (always succeeding) with atomic counters for how
// many times each of Subscribe/Unsubscribe was called, and exposes the
// resources it was constructed with so a test can trigger
// notifications/resources/updated on demand via srv.ResourceUpdated.
type subscribableFakeBackend struct {
	srv              *mcp.Server
	subscribeCount   atomic.Int32
	unsubscribeCount atomic.Int32
}

func newSubscribableFakeBackend(name string, uris ...string) *subscribableFakeBackend {
	f := &subscribableFakeBackend{}
	f.srv = mcp.NewServer(&mcp.Implementation{Name: name, Version: "v1"}, &mcp.ServerOptions{
		SubscribeHandler: func(context.Context, *mcp.SubscribeRequest) error {
			f.subscribeCount.Add(1)
			return nil
		},
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error {
			f.unsubscribeCount.Add(1)
			return nil
		},
	})
	for _, uri := range uris {
		f.srv.AddResource(&mcp.Resource{URI: uri, Name: uri},
			func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "content"}}}, nil
			})
	}
	return f
}

// newSubscriptionTestGateway connects to fb over HTTP, builds a
// *gateway.Server wired with a fresh SubscriptionRegistry and (if
// wireResourceUpdated) an OnResourceUpdated callback that relays into it,
// and returns the gateway's own HTTP endpoint plus a cleanup func. Shared
// setup for every subscription integration test below.
func newSubscriptionTestGateway(t *testing.T, fb *subscribableFakeBackend, wireResourceUpdated bool) (gwURL string, gw *gateway.Server, cleanup func()) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	httpA := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fb.srv }, nil))

	subs := gateway.NewSubscriptionRegistry()
	cb := backend.ChangeCallbacks{}
	if wireResourceUpdated {
		cb.OnResourceUpdated = func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			subs.Relay(ctx, gw.MCP(), logger, "backend-a", req.Params.URI)
		}
	}
	ctx := context.Background()
	connA, err := backend.Connect(ctx, config.BackendConfig{Name: "backend-a", Transport: "http", URL: httpA.URL}, cb)
	if err != nil {
		t.Fatalf("connect backend-a: %v", err)
	}

	resources, err := connA.ListResources(ctx)
	if err != nil {
		t.Fatalf("list backend-a resources: %v", err)
	}
	resourceNameOf := func(r *mcp.Resource) string { return r.URI }
	resourceRename := func(r *mcp.Resource, name string) *mcp.Resource { c := *r; c.URI = name; return &c }
	table := router.Resolve([]router.Entry[*mcp.Resource]{{BackendName: "backend-a", Items: resources}}, resourceNameOf, resourceRename, nil)

	gw = gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{"backend-a": connA},
		Tables:   gateway.Tables{Resources: table},
		Relays:   gateway.Relays{Subscriptions: subs},
	})

	gwHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return gw.MCP() }, nil))

	return gwHTTP.URL, gw, func() {
		gwHTTP.Close()
		_ = connA.Close()
		httpA.Close()
	}
}

// newSubscribedGatewayClient connects a client to gwURL and returns its
// session plus a channel receiving every notifications/resources/updated
// URI it sees.
func newSubscribedGatewayClient(t *testing.T, gwURL string) (session *mcp.ClientSession, updatedCh <-chan string) {
	t.Helper()
	ch := make(chan string, 8)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			ch <- req.Params.URI
		},
	})
	s, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: gwURL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	return s, ch
}

// TestGateway_SubscribeRefCountsUpstreamSubscribeUnsubscribe checks that
// mcprt issues exactly one upstream resources/subscribe on the first
// downstream subscriber, none on a second subscriber to the same URI, and
// exactly one upstream resources/unsubscribe only once the last downstream
// subscriber leaves.
func TestGateway_SubscribeRefCountsUpstreamSubscribeUnsubscribe(t *testing.T) {
	fb := newSubscribableFakeBackend("backend-a", "file:///a")
	gwURL, _, cleanup := newSubscriptionTestGateway(t, fb, false)
	defer cleanup()
	ctx := context.Background()

	session1, _ := newSubscribedGatewayClient(t, gwURL)
	defer func() { _ = session1.Close() }()
	session2, _ := newSubscribedGatewayClient(t, gwURL)
	defer func() { _ = session2.Close() }()

	if err := session1.Subscribe(ctx, &mcp.SubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("session1 Subscribe: %v", err)
	}
	if got := fb.subscribeCount.Load(); got != 1 {
		t.Fatalf("backend subscribeCount after first subscriber = %d, want 1", got)
	}

	if err := session2.Subscribe(ctx, &mcp.SubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("session2 Subscribe: %v", err)
	}
	if got := fb.subscribeCount.Load(); got != 1 {
		t.Fatalf("backend subscribeCount after second subscriber = %d, want 1 (no re-subscribe)", got)
	}

	if err := session1.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("session1 Unsubscribe: %v", err)
	}
	if got := fb.unsubscribeCount.Load(); got != 0 {
		t.Fatalf("backend unsubscribeCount with one subscriber remaining = %d, want 0", got)
	}

	if err := session2.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("session2 Unsubscribe: %v", err)
	}
	if got := fb.unsubscribeCount.Load(); got != 1 {
		t.Fatalf("backend unsubscribeCount after last subscriber leaves = %d, want 1", got)
	}
}

// TestGateway_ResourceUpdatedRelayedToSubscribedDownstream checks the
// spec's core integration scenario: a fake backend's real resource update
// reaches a subscribed downstream client via notifications/resources/
// updated.
func TestGateway_ResourceUpdatedRelayedToSubscribedDownstream(t *testing.T) {
	fb := newSubscribableFakeBackend("backend-a", "file:///a")
	gwURL, _, cleanup := newSubscriptionTestGateway(t, fb, true)
	defer cleanup()
	ctx := context.Background()

	session, updatedCh := newSubscribedGatewayClient(t, gwURL)
	defer func() { _ = session.Close() }()

	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("Subscribe(file:///a): %v", err)
	}

	if err := fb.srv.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: "file:///a"}); err != nil {
		t.Fatalf("backend ResourceUpdated: %v", err)
	}

	select {
	case uri := <-updatedCh:
		if uri != "file:///a" {
			t.Fatalf("downstream received update for %q, want file:///a", uri)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("downstream did not receive notifications/resources/updated within 5s")
	}
}

// TestGateway_UnsubscribedSessionStopsReceivingButOtherSessionStillDoes
// checks the spec's second integration scenario: two downstream sessions
// subscribed to the same URI, one unsubscribes, the other keeps receiving
// updates.
func TestGateway_UnsubscribedSessionStopsReceivingButOtherSessionStillDoes(t *testing.T) {
	fb := newSubscribableFakeBackend("backend-a", "file:///a")
	gwURL, _, cleanup := newSubscriptionTestGateway(t, fb, true)
	defer cleanup()
	ctx := context.Background()

	session1, updatedCh1 := newSubscribedGatewayClient(t, gwURL)
	defer func() { _ = session1.Close() }()
	session2, updatedCh2 := newSubscribedGatewayClient(t, gwURL)
	defer func() { _ = session2.Close() }()

	if err := session1.Subscribe(ctx, &mcp.SubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("session1 Subscribe: %v", err)
	}
	if err := session2.Subscribe(ctx, &mcp.SubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("session2 Subscribe: %v", err)
	}
	if err := session1.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("session1 Unsubscribe: %v", err)
	}

	if err := fb.srv.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: "file:///a"}); err != nil {
		t.Fatalf("backend ResourceUpdated: %v", err)
	}

	select {
	case uri := <-updatedCh2:
		if uri != "file:///a" {
			t.Fatalf("session2 received update for %q, want file:///a", uri)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session2 (still subscribed) did not receive the update within 5s")
	}

	select {
	case uri := <-updatedCh1:
		t.Fatalf("session1 (unsubscribed) received update for %q, want none", uri)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestGateway_SubscribeUnknownURIReturnsError checks that
// resources/subscribe for a URI not present in resourceTable returns an
// error instead of silently succeeding.
func TestGateway_SubscribeUnknownURIReturnsError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: map[string]*backend.Backend{},
		Relays:   gateway.Relays{Subscriptions: gateway.NewSubscriptionRegistry()},
	})
	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = session.Close() }()

	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: "file:///no-such"}); err == nil {
		t.Fatal("Subscribe(file:///no-such): got no error, want an error")
	}
}

// TestGateway_DownstreamDisconnectUnsubscribesUpstreamWhenLastSubscriberLeaves
// checks that a downstream client disconnecting without ever calling
// resources/unsubscribe still triggers an upstream resources/unsubscribe
// once it was the last subscriber -- exercising
// startSessionCloseWatcherOnce's session.Wait()-based cleanup.
func TestGateway_DownstreamDisconnectUnsubscribesUpstreamWhenLastSubscriberLeaves(t *testing.T) {
	fb := newSubscribableFakeBackend("backend-a", "file:///a")
	gwURL, _, cleanup := newSubscriptionTestGateway(t, fb, false)
	defer cleanup()
	ctx := context.Background()

	session, _ := newSubscribedGatewayClient(t, gwURL)
	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("Subscribe(file:///a): %v", err)
	}
	if got := fb.subscribeCount.Load(); got != 1 {
		t.Fatalf("backend subscribeCount = %d, want 1", got)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("closing downstream session: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fb.unsubscribeCount.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := fb.unsubscribeCount.Load(); got != 1 {
		t.Fatalf("backend unsubscribeCount after downstream disconnect = %d, want 1 (session close must trigger cleanup)", got)
	}
}
```

- [ ] **Step 7: 新規テストを実行する**

Run: `go test ./internal/gateway/... -run TestGateway_Subscribe -v` および `go test ./internal/gateway/... -run TestGateway_ResourceUpdatedRelayedToSubscribedDownstream -v` および `go test ./internal/gateway/... -run TestGateway_UnsubscribedSessionStopsReceiving -v` および `go test ./internal/gateway/... -run TestGateway_DownstreamDisconnect -v`
Expected: すべてPASS。

- [ ] **Step 8: `internal/gateway`パッケージ全体のテストを実行する**

Run: `go test ./internal/gateway/... -v 2>&1 | tail -100`
Expected: 既存テストも含めて全PASS（無回帰）。

- [ ] **Step 9: ビルド・vet・フォーマットを確認する**

Run: `go build ./... && go vet ./... && gofmt -l internal/gateway/gateway.go internal/gateway/gateway_test.go`
Expected: すべて成功、`gofmt -l`は何も出力しない。

- [ ] **Step 10: コミット**

```bash
git add internal/gateway/gateway.go internal/gateway/gateway_test.go
git commit -m "feat(gateway): wire resources/subscribe and resources/unsubscribe to SubscriptionRegistry"
```

---

## Task 4: `internal/cli/server.go`への配線とe2eテスト

**Files:**
- Modify: `internal/cli/server.go:289-292`（`buildGateway`の`gwH.relays`構築）, `:362-365`（`gwHolder`ドキュメント）, `:577-692`（`superviseBackend`）
- Test: `internal/cli/server_test.go`
- Modify: `README.md:96-99`

**Interfaces:**
- Consumes: Task 1〜3で追加した`gateway.NewSubscriptionRegistry`/`gateway.Relays.Subscriptions`/`(*SubscriptionRegistry).Relay`/`.BackendReconnected`、Task 2の`backend.ChangeCallbacks.OnResourceUpdated`。
- Produces: なし（このプランの最終タスク）。

- [ ] **Step 1: `buildGateway`で`SubscriptionRegistry`を構築する**

`internal/cli/server.go`の現在289-292行目：

```go
	gwH.relays = gateway.Relays{
		Progress: gateway.NewProgressRegistry(),
		Calls:    gateway.NewCallRouter(),
	}
```

を、以下に変更する：

```go
	gwH.relays = gateway.Relays{
		Progress:      gateway.NewProgressRegistry(),
		Calls:         gateway.NewCallRouter(),
		Subscriptions: gateway.NewSubscriptionRegistry(),
	}
```

- [ ] **Step 2: `gwHolder`のドキュメントコメントを更新する**

現在360行目付近の

```go
// (buildGateway always sets both of its
// fields; only some tests construct a bare gwHolder{} without them) means
// "no progress relay/elicitation routing for this generation," matching a
// nil *gateway.ProgressRegistry/*gateway.CallRouter everywhere else.
```

を

```go
// (buildGateway always sets all three of its
// fields; only some tests construct a bare gwHolder{} without them) means
// "no progress relay/elicitation routing/resource subscription for this
// generation," matching a nil *gateway.ProgressRegistry/*gateway.
// CallRouter/*gateway.SubscriptionRegistry everywhere else.
```

に変更する。

- [ ] **Step 3: `superviseBackend`に`OnResourceUpdated`配線と再接続後の再購読を追加する**

`internal/cli/server.go`の`superviseBackend`関数、現在585-617行目（`OnProgress`/`OnElicit`の配線ブロック）の末尾、`}`（617行目、`if gwH.relays.Calls != nil { ... }`ブロックの閉じ括弧）の直後に追加する：

```go
		if gwH.relays.Subscriptions != nil {
			cb.OnResourceUpdated = func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
				gw := gwH.ptr.Load()
				if gw == nil {
					return
				}
				gwH.relays.Subscriptions.Relay(ctx, gw.MCP(), logger, bc.Name, req.Params.URI)
			}
		}
```

次に、現在668-672行目：

```go
		if gw != nil {
			gw.ConnectBackend(bc.Name, c.backend, bc.Prefix, c.tools, c.resources, c.resourceTemplates, c.prompts)
		} else if onFirstConnect != nil {
			onFirstConnect(c)
		}
```

を、以下に変更する（再接続時の再購読は`gw != nil`のときだけ意味があり、その分岐に属する。設計書の`for _, c := range gwH.relays.Subscriptions.BackendReconnected(bc.Name)`はループ変数`c`が外側の`connectResult`の`c`と衝突し`c.backend`という存在しないフィールドを参照してしまうため、ループ変数名を`sub`に変え、外側の`c.backend`（今接続し直したばかりのbackend接続）を使う）：

```go
		if gw != nil {
			gw.ConnectBackend(bc.Name, c.backend, bc.Prefix, c.tools, c.resources, c.resourceTemplates, c.prompts)
			if gwH.relays.Subscriptions != nil {
				for _, sub := range gwH.relays.Subscriptions.BackendReconnected(bc.Name) {
					if err := c.backend.Session.Subscribe(ctx, &mcp.SubscribeParams{URI: sub.OriginalURI}); err != nil {
						logger.Warn("resubscribe after reconnect failed", "backend", bc.Name, "uri", sub.OriginalURI, "error", err)
					}
				}
			}
		} else if onFirstConnect != nil {
			onFirstConnect(c)
		}
```

- [ ] **Step 4: `go build`でコンパイルエラーを洗い出し、機械的に直す**

Run: `go build ./...`
Expected: エラーなくビルドできる。

- [ ] **Step 5: e2eテストを`internal/cli/server_test.go`に追加する**

ファイル冒頭の`import`に必要なものが既に揃っていることを確認する（`net`, `net/http`, `net/http/httptest`, `sort`, `time`, `context`, `fmt`は既存テストで使用済み）。`waitForToolNames`（532行目付近）の直後に、リソース版のポーリングヘルパーを追加する：

```go
// waitForResourceURIs polls session.Resources until it lists exactly want
// (order-independent), matching waitForToolNames' pattern for tools.
func waitForResourceURIs(t *testing.T, ctx context.Context, session *mcp.ClientSession, want []string) {
	t.Helper()
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)

	deadline := time.Now().Add(5 * time.Second)
	var lastGot []string
	for time.Now().Before(deadline) {
		var got []string
		for r, err := range session.Resources(ctx, nil) {
			if err != nil {
				t.Fatalf("listing resources: %v", err)
			}
			got = append(got, r.URI)
		}
		sort.Strings(got)
		lastGot = got
		if fmt.Sprint(got) == fmt.Sprint(wantSorted) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("resources never became %v within 5s (last seen: %v)", wantSorted, lastGot)
}
```

その後、ファイル末尾（`TestServerCommand_BackendReconnectsAfterDisconnect`の後）に、e2eテストを2件追加する：

```go
// TestServerCommand_RelaysResourceUpdatedToSubscribedDownstream checks the
// resource-subscription-relay feature end-to-end through the real server
// command: a downstream client's resources/subscribe reaches the backend,
// and the backend's notifications/resources/updated reaches the
// downstream client -- exercising the real production wiring
// (superviseBackend's OnResourceUpdated, SubscriptionRegistry.Relay).
func TestServerCommand_RelaysResourceUpdatedToSubscribedDownstream(t *testing.T) {
	backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, &mcp.ServerOptions{
		SubscribeHandler:   func(context.Context, *mcp.SubscribeRequest) error { return nil },
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
	})
	backendSrv.AddResource(&mcp.Resource{URI: "file:///a", Name: "a"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "content"}}}, nil
		})
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil))
	defer backendHTTP.Close()

	gatewayAddr := freePort(t)
	configPath := writeConfig(t, fmt.Sprintf(`
listen:
  http: %q

backends:
  - name: fake
    transport: http
    url: %q
`, gatewayAddr, backendHTTP.URL))

	ctx, cancel := context.WithCancel(context.Background())
	execErr := make(chan error, 1)
	go func() {
		execErr <- cli.Execute(ctx, []string{"server", "--config", configPath})
	}()

	updatedCh := make(chan string, 4)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updatedCh <- req.Params.URI
		},
	})
	var session *mcp.ClientSession
	var connectErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, connectErr = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + gatewayAddr}, nil)
		if connectErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if connectErr != nil {
		t.Fatalf("connecting to gateway: %v", connectErr)
	}
	waitForResourceURIs(t, ctx, session, []string{"file:///a"})

	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("Subscribe(file:///a): %v", err)
	}
	if err := backendSrv.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: "file:///a"}); err != nil {
		t.Fatalf("backend ResourceUpdated: %v", err)
	}

	select {
	case uri := <-updatedCh:
		if uri != "file:///a" {
			t.Fatalf("downstream received update for %q, want file:///a", uri)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("downstream did not receive notifications/resources/updated within 5s")
	}

	_ = session.Close()
	cancel()
	if err := <-execErr; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}
}

// TestServerCommand_SubscriptionSurvivesBackendReconnect checks that a
// downstream subscription automatically resumes after the owning backend
// disconnects and reconnects, following the same real-listener-restart
// technique TestServerCommand_BackendReconnectsAfterDisconnect already
// uses (see its comments for the SSE-retry-backoff timing rationale this
// test's deadlines mirror).
func TestServerCommand_SubscriptionSurvivesBackendReconnect(t *testing.T) {
	var subscribeCount atomic.Int32
	newBackendHandler := func() http.Handler {
		backendSrv := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "v1"}, &mcp.ServerOptions{
			SubscribeHandler: func(context.Context, *mcp.SubscribeRequest) error {
				subscribeCount.Add(1)
				return nil
			},
			UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
		})
		backendSrv.AddResource(&mcp.Resource{URI: "file:///a", Name: "a"},
			func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "content"}}}, nil
			})
		return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backendSrv }, nil)
	}

	backendAddr := freePort(t)
	backendListener, err := net.Listen("tcp", backendAddr)
	if err != nil {
		t.Fatalf("listening on %s: %v", backendAddr, err)
	}
	backendHTTP := &http.Server{Handler: newBackendHandler()}
	go func() { _ = backendHTTP.Serve(backendListener) }()
	defer func() { _ = backendHTTP.Close() }()

	gatewayAddr := freePort(t)
	configPath := writeConfig(t, fmt.Sprintf(`
listen:
  http: %q

backends:
  - name: fake
    transport: http
    url: %q
`, gatewayAddr, "http://"+backendAddr))

	ctx, cancel := context.WithCancel(context.Background())
	execErr := make(chan error, 1)
	go func() {
		execErr <- cli.Execute(ctx, []string{"server", "--config", configPath})
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	var session *mcp.ClientSession
	var connectErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, connectErr = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + gatewayAddr}, nil)
		if connectErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if connectErr != nil {
		t.Fatalf("connecting to gateway: %v", connectErr)
	}
	waitForResourceURIs(t, ctx, session, []string{"file:///a"})

	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: "file:///a"}); err != nil {
		t.Fatalf("Subscribe(file:///a): %v", err)
	}
	if got := subscribeCount.Load(); got != 1 {
		t.Fatalf("backend subscribeCount before disconnect = %d, want 1", got)
	}

	if err := backendHTTP.Close(); err != nil {
		t.Fatalf("stopping backend: %v", err)
	}

	deadline = time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		var got []string
		for r, err := range session.Resources(ctx, nil) {
			if err != nil {
				t.Fatalf("listing resources: %v", err)
			}
			got = append(got, r.URI)
		}
		if len(got) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Restart a backend listening on the SAME address, with a fresh
	// SubscribeHandler (subscribeCount is shared across restarts via the
	// closure above): superviseBackend's unbounded retry loop reconnects
	// automatically, and its post-ConnectBackend resubscribe logic must
	// re-issue resources/subscribe for file:///a on the new connection.
	backendListener2, err := net.Listen("tcp", backendAddr)
	if err != nil {
		t.Fatalf("re-listening on %s: %v", backendAddr, err)
	}
	backendHTTP2 := &http.Server{Handler: newBackendHandler()}
	go func() { _ = backendHTTP2.Serve(backendListener2) }()
	defer func() { _ = backendHTTP2.Close() }()

	waitForResourceURIs(t, ctx, session, []string{"file:///a"})

	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if subscribeCount.Load() == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := subscribeCount.Load(); got != 2 {
		t.Fatalf("backend subscribeCount after reconnect = %d, want 2 (must auto-resubscribe file:///a)", got)
	}

	_ = session.Close()
	cancel()
	if err := <-execErr; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}
}
```

（ファイル冒頭の`import`に`sync/atomic`が無ければ追加する。）

- [ ] **Step 6: 新規e2eテストを実行する**

Run: `go test ./internal/cli/... -run TestServerCommand_RelaysResourceUpdatedToSubscribedDownstream -v -timeout 60s`
Expected: PASS。

Run: `go test ./internal/cli/... -run TestServerCommand_SubscriptionSurvivesBackendReconnect -v -timeout 120s`
Expected: PASS（`TestServerCommand_BackendReconnectsAfterDisconnect`と同様、SSEの再接続バックオフにより数十秒かかる）。

- [ ] **Step 7: README.mdを更新する**

`README.md`の現在98-99行目：

```
a prefix onto one would produce an invalid URI. `resources/subscribe` and
`notifications/resources/updated` are not relayed.
```

を、以下に置き換える：

```
a prefix onto one would produce an invalid URI. `resources/subscribe`/
`resources/unsubscribe` are forwarded to the backend that owns the resource
(reference-counted: mcprt subscribes upstream once, on the first downstream
subscriber, and unsubscribes once the last one leaves), and a backend's
`notifications/resources/updated` is relayed to every downstream session
currently subscribed to that URI. Subscribing to a resource *template*'s
dynamically-generated URIs is not supported -- only URIs registered as
exact resources can be subscribed to. A subscription does not survive
`mcprt server` restarting or reloading via SIGHUP (matching every other
in-memory state mcprt holds), but does survive the owning backend
disconnecting and reconnecting: mcprt automatically re-subscribes every URI
that was subscribed before the disconnect.
```

- [ ] **Step 8: パッケージ全体・モジュール全体のテストとビルドを確認する**

Run: `go build ./... && go vet ./... && gofmt -l internal/cli/server.go internal/cli/server_test.go README.md`
Expected: すべて成功、`gofmt -l`は何も出力しない（`README.md`は`gofmt`の対象外なのでこのコマンドからは自然に除外される；対象拡張子でなければ`gofmt -l`はエラーを出す可能性があるため、`.go`ファイルのみに絞って実行する: `gofmt -l internal/cli/server.go internal/cli/server_test.go`）。

Run: `go test ./... -timeout 300s`
Expected: モジュール全体が無回帰でPASSする。

- [ ] **Step 9: コミット**

```bash
git add internal/cli/server.go internal/cli/server_test.go README.md
git commit -m "feat(cli): relay resources/subscribe to the owning backend, with reconnect resubscribe"
```

## Self-Review Notes

- **Spec coverage:** `resources/subscribe`/`unsubscribe`の参照カウント中継（Task 3の`subscribeHandler`/`unsubscribeHandler`、Task 3のTest 1でカウンタ検証）、`notifications/resources/updated`のリレー（Task 1の`Relay`、Task 3のTest 2・3）、backend再接続時の自動再購読（Task 4のTest 2、`superviseBackend`の修正版再購読コード）、downstreamセッション切断時のクリーンアップ（Task 3の`startSessionCloseWatcherOnce`、Test 5）、未知URIのエラー処理（Task 3のTest 4）、リソーステンプレートは対象外（`subscribeHandler`が`resourceTemplateTable`を一切参照しないことで担保、Global Constraintsに明記）、購読状態を永続化しないこと（新規状態はすべて`gateway.Server`/`SubscriptionRegistry`のインメモリフィールドのみで、設定ファイルやディスクへの書き出しを一切行わないことで自然に満たされる — 専用のテストは追加しない、既存のSIGHUP再読み込みが`buildGateway`を都度呼び直すことで新しい`SubscriptionRegistry`が作られる既存の仕組みに乗っている）。ログ方針（成功時は無音、`BackendReconnected`後の再購読失敗のみ`logger.Warn`）はTask 4のコード自体に明記、専用の単体テストは追加しない（`ProgressRegistry`の先例も同様にログ出力の単体テストは持たない）。ギャップなし。
- **Placeholder scan:** 各ステップに実コードまたは実行可能なコマンドを記載済み。「TODO」「後で」等の記述なし。
- **Type consistency:** `SubscriptionRegistry.Subscribe`/`Unsubscribe`/`SessionClosed`/`BackendReconnected`/`Relay`のシグネチャはTask 1で定義した通りにTask 3・4で一貫して使用している（`subscriptionToClose{BackendName, OriginalURI string}`のフィールド名も全タスクで一致）。`gateway.Relays.Subscriptions`のフィールド名はTask 3で定義し、Task 4の`buildGateway`修正でそのまま使用している。
