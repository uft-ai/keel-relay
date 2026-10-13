package usage

import (
	"context"
	"testing"
	"time"
)

// reusedGin stands in for a pooled gin.Context: the next request rewrites it.
type reusedGin struct{ values map[string]any }

func (g *reusedGin) Get(key string) (any, bool) {
	value, ok := g.values[key]
	return value, ok
}

// releasedGin fails the test when read: its request has ended.
type releasedGin struct{ t *testing.T }

func (g releasedGin) Get(key string) (any, bool) {
	g.t.Errorf("read %q from a released Gin context", key)
	return nil, false
}

func ginContext(values map[string]any) (context.Context, *reusedGin) {
	gin := &reusedGin{values: values}
	return context.WithValue(context.Background(), "gin", gin), gin
}

func publishThrough(t *testing.T, ctx context.Context, record Record) Record {
	t.Helper()
	mgr := NewManager(10)
	t.Cleanup(mgr.Stop)
	plugin := &captureUsagePlugin{records: make(chan Record, 1)}
	mgr.Register(plugin)
	mgr.Publish(ctx, record)
	select {
	case got := <-plugin.records:
		return got
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for usage record")
		return Record{}
	}
}

func TestRequestIdentityOutlivesTheGinContext(t *testing.T) {
	ctx, gin := ginContext(map[string]any{"userApiKey": "sk-first", "accessProvider": "config-inline"})
	ctx = WithRequestIdentity(ctx, nil)
	gin.values = map[string]any{"userApiKey": "sk-next", "accessProvider": "custom"}

	got := publishThrough(t, ctx, Record{Provider: "openai"})
	if got.APIKey != "sk-first" || got.AccessProvider != "config-inline" || !got.IsNativeKey {
		t.Fatalf("record = key %q provider %q native %v, want the first request's identity", got.APIKey, got.AccessProvider, got.IsNativeKey)
	}
}

func TestRequestIdentityKeepsAnEmptyIdentityEmpty(t *testing.T) {
	ctx, gin := ginContext(map[string]any{})
	ctx = WithRequestIdentity(ctx, nil)
	gin.values = map[string]any{"userApiKey": "sk-next", "accessProvider": "config-inline"}

	got := publishThrough(t, ctx, Record{Provider: "openai"})
	if got.APIKey != "" || got.AccessProvider != "" || got.IsNativeKey {
		t.Fatalf("record = key %q provider %q native %v, want no identity", got.APIKey, got.AccessProvider, got.IsNativeKey)
	}
}

func TestRequestIdentityPrefersTypedValues(t *testing.T) {
	ctx, _ := ginContext(map[string]any{"userApiKey": "sk-gin", "accessProvider": "gin-provider"})
	ctx = WithAPIKey(ctx, "sk-typed")
	ctx = WithAccessProvider(ctx, "config-inline")
	ctx = WithIsNativeKey(ctx, false)
	ctx = WithRequestIdentity(ctx, nil)

	got := publishThrough(t, ctx, Record{Provider: "openai"})
	if got.APIKey != "sk-typed" || got.AccessProvider != "config-inline" {
		t.Fatalf("record = key %q provider %q, want typed values", got.APIKey, got.AccessProvider)
	}
	if got.IsNativeKey {
		t.Fatal("explicit native=false was replaced by the provider's default")
	}
}

func TestRequestIdentityCarriesTheRequestContextToADetachedParent(t *testing.T) {
	request := WithAPIKey(context.Background(), "sk-request")
	request = WithAccessProvider(request, "config-inline")
	request = WithIsNativeKey(request, false)
	ctx, gin := ginContext(map[string]any{"userApiKey": "sk-gin", "accessProvider": "gin-provider"})
	ctx = WithRequestIdentity(ctx, request)
	gin.values = map[string]any{}

	got := publishThrough(t, ctx, Record{Provider: "openai"})
	if got.APIKey != "sk-request" || got.AccessProvider != "config-inline" || got.IsNativeKey {
		t.Fatalf("record = key %q provider %q native %v, want the request context's identity", got.APIKey, got.AccessProvider, got.IsNativeKey)
	}
}

func TestRecordIdentityWithoutASnapshotStillReadsGin(t *testing.T) {
	ctx, _ := ginContext(map[string]any{"userApiKey": "sk-legacy", "accessProvider": "config-api-key"})

	got := publishThrough(t, ctx, Record{Provider: "openai"})
	if got.APIKey != "sk-legacy" || got.AccessProvider != "config-api-key" || !got.IsNativeKey {
		t.Fatalf("record = key %q provider %q native %v, want the legacy Gin fallback", got.APIKey, got.AccessProvider, got.IsNativeKey)
	}
}

func TestRecordIdentityKeepsACompleteRecord(t *testing.T) {
	ctx := context.WithValue(context.Background(), "gin", releasedGin{t})
	record := ResolveRecordIdentity(ctx, Record{APIKey: "sk-set", AccessProvider: "custom", IsNativeKey: true})
	if record.APIKey != "sk-set" || record.AccessProvider != "custom" || !record.IsNativeKey {
		t.Fatalf("record = %+v, want it unchanged", record)
	}
	if empty := ResolveRecordIdentity(nil, Record{}); empty.APIKey != "" || empty.AccessProvider != "" || empty.IsNativeKey {
		t.Fatalf("nil context record = %+v, want empty", empty)
	}
}
