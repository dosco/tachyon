package proxy

import (
	"net"
	"strings"
	"time"

	"tachyon/http1"
	irt "tachyon/internal/intent/runtime"
	"tachyon/internal/traffic"
	"tachyon/metrics"
)

type h1IntentView struct {
	req      *http1.Request
	host     string
	path     string
	clientIP string
}

func (v h1IntentView) Method() string   { return string(v.req.MethodBytes()) }
func (v h1IntentView) Path() string     { return v.path }
func (v h1IntentView) Host() string     { return v.host }
func (v h1IntentView) ClientIP() string { return v.clientIP }
func (v h1IntentView) Header(name string) string {
	return string(v.req.Lookup([]byte(name)))
}
func (v h1IntentView) Query(name string) string {
	q := strings.IndexByte(v.path, '?')
	if q < 0 {
		return ""
	}
	return rawQueryLookup(v.path[q+1:], name)
}
func (v h1IntentView) Cookie(name string) string {
	return rawCookieLookup(string(v.req.Lookup([]byte("cookie"))), name)
}

type staticIntentView struct {
	method   string
	path     string
	host     string
	clientIP string
	fields   []headerKV
}

func (v staticIntentView) Method() string   { return v.method }
func (v staticIntentView) Path() string     { return v.path }
func (v staticIntentView) Host() string     { return v.host }
func (v staticIntentView) ClientIP() string { return v.clientIP }
func (v staticIntentView) Header(name string) string {
	for _, f := range v.fields {
		if strings.EqualFold(f.name, name) {
			return f.value
		}
	}
	return ""
}
func (v staticIntentView) Query(name string) string {
	q := strings.IndexByte(v.path, '?')
	if q < 0 {
		return ""
	}
	return rawQueryLookup(v.path[q+1:], name)
}
func (v staticIntentView) Cookie(string) string { return "" }

// rawQueryLookup extracts the first value of a named param from a raw query string
// ("key=val&key2=val2") without allocating.
func rawQueryLookup(query, name string) string {
	for query != "" {
		var pair string
		pair, query, _ = strings.Cut(query, "&")
		k, v, _ := strings.Cut(pair, "=")
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// rawCookieLookup extracts a named cookie value from a Cookie header value
// ("name=val; name2=val2") without additional allocations.
func rawCookieLookup(header, name string) string {
	for header != "" {
		var pair string
		pair, header, _ = strings.Cut(header, ";")
		pair = strings.TrimSpace(pair)
		k, v, _ := strings.Cut(pair, "=")
		if strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func intentHeaderRemoved(muts irt.HeaderMutations, name []byte) bool {
	return muts.Removed(string(name))
}

func intentHeaderOverridden(muts irt.HeaderMutations, name []byte) bool {
	return muts.Overridden(string(name))
}

func sendTerminal(c net.Conn, tr irt.TerminalResponse) {
	var buf [1024]byte
	w := http1.AppendStatus(buf[:0], tr.Status)
	tr.Headers.Each(func(hm irt.HeaderMutation) bool {
		if hm.Remove || hm.Name == "" {
			return true
		}
		w = http1.AppendHeader(w, []byte(hm.Name), []byte(hm.Value))
		return true
	})
	w = http1.AppendContentLength(w, int64(len(tr.Body)))
	w = http1.AppendEndOfHeaders(w)
	if tr.Body != "" {
		w = append(w, tr.Body...)
	}
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write(w)
	metrics.RecordStatus(tr.Status)
}

func applyHeaderMutations(fields []headerKV, muts irt.HeaderMutations) []headerKV {
	out := fields[:0]
	for _, field := range fields {
		if intentHeaderRemoved(muts, []byte(field.name)) || intentHeaderOverridden(muts, []byte(field.name)) {
			continue
		}
		out = append(out, field)
	}
	muts.Each(func(hm irt.HeaderMutation) bool {
		if hm.Remove || hm.Name == "" {
			return true
		}
		out = append(out, headerKV{name: hm.Name, value: hm.Value})
		return true
	})
	return out
}

type headerKV struct {
	name  string
	value string
}

func recordTraffic(req *http1.Request, host, clientIP string, routeID, status int, trace irt.Trace) {
	if !traffic.Enabled() {
		return
	}
	headers := make(map[string]string, req.NumHeaders)
	src := req.Src()
	for i := 0; i < req.NumHeaders; i++ {
		headers[string(req.Headers[i].Name.Bytes(src))] = string(req.Headers[i].Value.Bytes(src))
	}
	traffic.Write(traffic.Record{
		Method:   string(req.MethodBytes()),
		Host:     host,
		Path:     string(req.PathBytes()),
		Headers:  headers,
		ClientIP: clientIP,
		Status:   status,
		RouteID:  routeID,
		Trace:    trace,
	})
}
