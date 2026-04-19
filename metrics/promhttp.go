package metrics

import (
	"io"
	"strconv"
)

// WritePrometheus emits the counter set in Prometheus text-exposition
// format (version 0.0.4). No external deps — the format is trivial
// enough that pulling in prometheus/client_golang for this would be
// overkill, and we don't want the alloc cost on scrape.
//
// The schema is flat and tag-light by design. Per-route breakdowns
// belong in log-based aggregation; the proxy itself just reports the
// basics operators need to know it's healthy.
func WritePrometheus(w io.Writer) error {
	s := Read()
	var buf []byte

	buf = appendGauge(buf,
		"tachyon_requests_total",
		"Total requests accepted by this worker.",
		"counter",
		s.Requests)
	buf = appendLabelled(buf,
		"tachyon_responses_total",
		"Responses returned to client, by status class.",
		"counter",
		[]labelled{
			{code: "2xx", v: s.OK2xx},
			{code: "4xx", v: s.Err4xx},
			{code: "5xx", v: s.Err5xx},
		})
	buf = appendLabelled(buf,
		"tachyon_upstream_errors_total",
		"Upstream failures this worker observed, by phase.",
		"counter",
		[]labelled{
			{code: "dial", v: s.UpDialErr},
			{code: "write", v: s.UpWriteErr},
			{code: "read", v: s.UpReadErr},
		})

	_, err := w.Write(buf)
	return err
}

type labelled struct {
	code string
	v    uint64
}

func appendGauge(dst []byte, name, help, typ string, v uint64) []byte {
	dst = append(dst, "# HELP "...)
	dst = append(dst, name...)
	dst = append(dst, ' ')
	dst = append(dst, help...)
	dst = append(dst, '\n')
	dst = append(dst, "# TYPE "...)
	dst = append(dst, name...)
	dst = append(dst, ' ')
	dst = append(dst, typ...)
	dst = append(dst, '\n')
	dst = append(dst, name...)
	dst = append(dst, ' ')
	dst = strconv.AppendUint(dst, v, 10)
	dst = append(dst, '\n')
	return dst
}

func appendLabelled(dst []byte, name, help, typ string, rows []labelled) []byte {
	dst = append(dst, "# HELP "...)
	dst = append(dst, name...)
	dst = append(dst, ' ')
	dst = append(dst, help...)
	dst = append(dst, '\n')
	dst = append(dst, "# TYPE "...)
	dst = append(dst, name...)
	dst = append(dst, ' ')
	dst = append(dst, typ...)
	dst = append(dst, '\n')
	for _, r := range rows {
		dst = append(dst, name...)
		dst = append(dst, `{code="`...)
		dst = append(dst, r.code...)
		dst = append(dst, `"} `...)
		dst = strconv.AppendUint(dst, r.v, 10)
		dst = append(dst, '\n')
	}
	return dst
}
