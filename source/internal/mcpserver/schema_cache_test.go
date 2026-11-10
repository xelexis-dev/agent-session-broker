package mcpserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

type schemaMeasurement struct {
	RSSKiB                   int64
	Projects                 int
	HeapAlloc, HeapInuse     int64
	CachePresent             bool
	Types, Schemas           int
	WarmTypes, WarmSchemas   int
	GrowthAlloc, GrowthInuse int64
}

// The pinned SDK has no public cache metrics; this never changes its maps.
func schemaCacheCounts(t *testing.T, cache *mcp.SchemaCache) (int, int) {
	t.Helper()
	if cache == nil {
		return 0, 0
	}
	counts := [2]int{}
	for i, name := range []string{"byType", "bySchema"} {
		field := reflect.ValueOf(cache).Elem().FieldByName(name)
		if !field.IsValid() || field.Type() != reflect.TypeFor[sync.Map]() {
			t.Fatal("SDK cache layout changed")
		}
		m := (*sync.Map)(unsafe.Pointer(field.UnsafeAddr()))
		m.Range(func(_, _ any) bool { counts[i]++; return true })
	}
	return counts[0], counts[1]
}
func schemaServerCache(server *mcp.Server) *mcp.SchemaCache {
	field := reflect.ValueOf(server).Elem().FieldByName("opts").FieldByName("SchemaCache")
	return (*mcp.SchemaCache)(field.UnsafePointer())
}
func schemaGC() runtime.MemStats {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
}
func TestSchemaCacheProbe(t *testing.T) {
	mode := os.Getenv("ASB_SCHEMA_PROBE")
	if mode == "" {
		t.Skip("subprocess heap probe")
	}
	n, e := strconv.Atoi(os.Getenv("ASB_SCHEMA_PROJECTS"))
	if e != nil || n < 1 {
		t.Fatal("probe count")
	}
	backend := broker.NewRemoteService()
	defer backend.Close()
	before := schemaGC()
	servers := make([]*mcp.Server, n)
	for i := range servers {
		servers[i] = New(backend, &stubMemory{})
	}
	after := schemaGC()
	cache := schemaServerCache(servers[0])
	types, schemas := schemaCacheCounts(t, cache)
	v := schemaMeasurement{Projects: n, HeapAlloc: int64(after.HeapAlloc) - int64(before.HeapAlloc), HeapInuse: int64(after.HeapInuse) - int64(before.HeapInuse), CachePresent: cache != nil, Types: types, Schemas: schemas}
	if runtime.GOOS == "darwin" {
		out, e := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
		if e != nil {
			t.Fatal("RSS sample")
		}
		v.RSSKiB, e = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if e != nil {
			t.Fatal("RSS decode")
		}
	}
	runtime.KeepAlive(servers)
	if mode == "churn" {
		// Warm both builder branches, then release all server/handler closures.
		New(stubBackend{}, &stubMemory{})
		servers = nil
		warm := schemaGC()
		v.WarmTypes, v.WarmSchemas = schemaCacheCounts(t, cache)
		for i := 0; i < 300; i++ {
			New(backend, &stubMemory{})
			if i%2 == 0 {
				New(stubBackend{}, &stubMemory{})
			}
		}
		cold := schemaGC()
		v.Types, v.Schemas = schemaCacheCounts(t, cache)
		v.GrowthAlloc = int64(cold.HeapAlloc) - int64(warm.HeapAlloc)
		v.GrowthInuse = int64(cold.HeapInuse) - int64(warm.HeapInuse)
	}
	raw, _ := json.Marshal(v)
	fmt.Printf("SCHEMA_MEASURE %s\n", raw)
}
func schemaProbe(t *testing.T, mode string, n int) schemaMeasurement {
	t.Helper()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(exe, "-test.run=^TestSchemaCacheProbe$", "-test.v")
	cmd.Env = append(os.Environ(), "ASB_SCHEMA_PROBE="+mode, "ASB_SCHEMA_PROJECTS="+strconv.Itoa(n))
	out, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("probe: %s", out)
	}
	scan := bufio.NewScanner(strings.NewReader(string(out)))
	for scan.Scan() {
		if raw, ok := strings.CutPrefix(scan.Text(), "SCHEMA_MEASURE "); ok {
			var v schemaMeasurement
			if json.Unmarshal([]byte(raw), &v) != nil {
				t.Fatal("probe decode")
			}
			return v
		}
	}
	t.Fatal("missing measurement")
	return schemaMeasurement{}
}
func TestSchemaCacheProjectHeap(t *testing.T) {
	one, ten := schemaProbe(t, "heap", 1), schemaProbe(t, "heap", 10)
	t.Logf("one=%+v ten=%+v", one, ten)
	if ten.HeapAlloc > 3*one.HeapAlloc+(1<<20) {
		t.Fatalf("project schemas retain roughly linear heap: one=%d ten=%d", one.HeapAlloc, ten.HeapAlloc)
	}
}
func TestSchemaCacheProjectChurn(t *testing.T) {
	v := schemaProbe(t, "churn", 10)
	t.Logf("churn300=%+v", v)
	if !v.CachePresent || v.Types == 0 {
		t.Fatal("project builders have no shared schema cache")
	}
	if v.Types != v.WarmTypes || v.Schemas != v.WarmSchemas {
		t.Fatal("cache entries grow with project churn")
	}
	if v.GrowthAlloc > 1<<20 || v.GrowthInuse > 2<<20 {
		t.Fatal("dropped project servers retain growing heap")
	}
}
