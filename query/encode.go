package query

import (
	"io"
	"net/http"

	"github.com/influxdata/flux"
)

const DialectType = "null"

// AddDialectMappings adds the null dialect mapping.
func AddDialectMappings(mappings flux.DialectMappings) error {
	return mappings.Add(DialectType, func() flux.Dialect {
		return NewNullDialect()
	})
}

type NullDialect struct{}

func NewNullDialect() *NullDialect {
	return &NullDialect{}
}

func (d *NullDialect) Encoder() flux.MultiResultEncoder {
	return &NullEncoder{}
}

func (d *NullDialect) DialectType() flux.DialectType {
	return DialectType
}

func (d *NullDialect) SetHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
}

type NullEncoder struct {
	flux.MultiResultEncoder
}

func (e *NullEncoder) Encode(w io.Writer, results flux.ResultIterator) (int64, error) {
	defer results.Release()
	// Consume and discard results.
	for results.More() {
		if err := results.Next().Tables().Do(func(tbl flux.Table) error {
			return tbl.Do(func(cr flux.ColReader) error {
				cr.Release()
				return nil
			})
		}); err != nil {
			return 0, err
		}
	}
	n, err := w.Write([]byte("null"))
	return int64(n), err
}
