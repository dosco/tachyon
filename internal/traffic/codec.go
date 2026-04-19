package traffic

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
)

func ReadAll(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	dec := json.NewDecoder(gz)
	var out []Record
	for {
		var rec Record
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}
