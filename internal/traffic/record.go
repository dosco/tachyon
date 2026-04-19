package traffic

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"time"

	irt "tachyon/internal/intent/runtime"
)

const EnvRecordOut = "TACHYON_RECORD_OUT"

type Record struct {
	Timestamp time.Time         `json:"timestamp"`
	ID        uint64            `json:"id"`
	Method    string            `json:"method"`
	Host      string            `json:"host"`
	Path      string            `json:"path"`
	Headers   map[string]string `json:"headers,omitempty"`
	ClientIP  string            `json:"client_ip,omitempty"`
	Status    int               `json:"status,omitempty"`
	RouteID   int               `json:"route_id"`
	Trace     irt.Trace         `json:"trace"`
}

type recorder struct {
	mu  sync.Mutex
	f   *os.File
	gz  *gzip.Writer
	enc *json.Encoder
	seq atomic.Uint64
}

var global recorder

func Enable(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	global.mu.Lock()
	defer global.mu.Unlock()
	global.f = f
	global.gz = gz
	global.enc = json.NewEncoder(gz)
	global.seq.Store(0)
	return nil
}

func Enabled() bool {
	global.mu.Lock()
	defer global.mu.Unlock()
	return global.enc != nil
}

func Close() error {
	global.mu.Lock()
	defer global.mu.Unlock()
	if global.enc == nil {
		return nil
	}
	err1 := global.gz.Close()
	err2 := global.f.Close()
	global.enc = nil
	global.gz = nil
	global.f = nil
	if err1 != nil {
		return err1
	}
	return err2
}

func Write(rec Record) {
	global.mu.Lock()
	defer global.mu.Unlock()
	if global.enc == nil {
		return
	}
	rec.ID = global.seq.Add(1)
	rec.Timestamp = time.Now().UTC()
	_ = global.enc.Encode(rec)
	_ = global.gz.Flush()
}
