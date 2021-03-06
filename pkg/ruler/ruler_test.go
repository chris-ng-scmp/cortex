package ruler

import (
	"context"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/notifier"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage"
	"github.com/stretchr/testify/assert"
	"github.com/weaveworks/common/user"

	"github.com/cortexproject/cortex/pkg/ingester/client"
	"github.com/cortexproject/cortex/pkg/ring"
	"github.com/cortexproject/cortex/pkg/ring/kv/consul"
	"github.com/cortexproject/cortex/pkg/ruler/rules"
	"github.com/cortexproject/cortex/pkg/util/flagext"
)

func defaultRulerConfig(store rules.RuleStore) (Config, func()) {
	// Create a new temporary directory for the rules, so that
	// each test will run in isolation.
	rulesDir, _ := ioutil.TempDir("/tmp", "ruler-tests")
	codec := ring.GetCodec()
	consul := consul.NewInMemoryClient(codec)
	cfg := Config{}
	flagext.DefaultValues(&cfg)
	cfg.RulePath = rulesDir
	cfg.StoreConfig.mock = store
	cfg.Ring.KVStore.Mock = consul
	cfg.Ring.NumTokens = 1
	cfg.Ring.ListenPort = 0
	cfg.Ring.InstanceAddr = "localhost"
	cfg.Ring.InstanceID = "localhost"

	// Create a cleanup function that will be called at the end of the test
	cleanup := func() {
		defer os.RemoveAll(rulesDir)
	}

	return cfg, cleanup
}

func newTestRuler(t *testing.T, cfg Config) *Ruler {
	engine := promql.NewEngine(promql.EngineOpts{
		MaxSamples:    1e6,
		MaxConcurrent: 20,
		Timeout:       2 * time.Minute,
	})

	noopQueryable := storage.QueryableFunc(func(ctx context.Context, mint, maxt int64) (storage.Querier, error) {
		return storage.NoopQuerier(), nil
	})

	// Mock the pusher
	pusher := newPusherMock()
	pusher.MockPush(&client.WriteResponse{}, nil)

	l := log.NewLogfmtLogger(os.Stdout)
	l = level.NewFilter(l, level.AllowInfo())
	ruler, err := NewRuler(cfg, engine, noopQueryable, pusher, prometheus.NewRegistry(), l)
	require.NoError(t, err)

	// Ensure all rules are loaded before usage
	ruler.loadRules(context.Background())

	return ruler
}

func TestNotifierSendsUserIDHeader(t *testing.T) {
	var wg sync.WaitGroup

	// We do expect 1 API call for the user create with the getOrCreateNotifier()
	wg.Add(1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, _, err := user.ExtractOrgIDFromHTTPRequest(r)
		assert.NoError(t, err)
		assert.Equal(t, userID, "1")
		wg.Done()
	}))
	defer ts.Close()

	// We create an empty rule store so that the ruler will not load any rule from it.
	cfg, cleanup := defaultRulerConfig(newMockRuleStore(nil))
	defer cleanup()

	err := cfg.AlertmanagerURL.Set(ts.URL)
	require.NoError(t, err)
	cfg.AlertmanagerDiscovery = false

	r := newTestRuler(t, cfg)
	defer r.Stop()
	n, err := r.getOrCreateNotifier("1")
	require.NoError(t, err)

	for _, not := range r.notifiers {
		defer not.stop()
	}
	// Loop until notifier discovery syncs up
	for len(n.Alertmanagers()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	n.Send(&notifier.Alert{
		Labels: labels.Labels{labels.Label{Name: "alertname", Value: "testalert"}},
	})

	wg.Wait()
}

func TestRuler_Rules(t *testing.T) {
	cfg, cleanup := defaultRulerConfig(newMockRuleStore(mockRules))
	defer cleanup()

	r := newTestRuler(t, cfg)
	defer r.Stop()

	// test user1
	ctx := user.InjectOrgID(context.Background(), "user1")
	rls, err := r.Rules(ctx, &RulesRequest{})
	require.NoError(t, err)
	require.Len(t, rls.Groups, 1)
	rg := rls.Groups[0]
	expectedRg := mockRules["user1"][0]
	compareRuleGroupDescs(t, rg, expectedRg)

	// test user2
	ctx = user.InjectOrgID(context.Background(), "user2")
	rls, err = r.Rules(ctx, &RulesRequest{})
	require.NoError(t, err)
	require.Len(t, rls.Groups, 1)
	rg = rls.Groups[0]
	expectedRg = mockRules["user2"][0]
	compareRuleGroupDescs(t, rg, expectedRg)
}

func compareRuleGroupDescs(t *testing.T, expected, got *rules.RuleGroupDesc) {
	require.Equal(t, expected.Name, got.Name)
	require.Equal(t, expected.Namespace, got.Namespace)
	require.Len(t, got.Rules, len(expected.Rules))
	for i := range got.Rules {
		require.Equal(t, expected.Rules[i].Record, got.Rules[i].Record)
		require.Equal(t, expected.Rules[i].Alert, got.Rules[i].Alert)
	}
}
