package admin

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"
)

// redacted replaces a secret in GET /config.
const redacted = "[redacted]"

// config serves the current runtime's effective config: the file as loaded,
// with defaults filled in.
func (h *Handler) config(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	if err := encodeConfig(&buf, reflect.ValueOf(h.cur.Load().Config)); err != nil {
		h.log.Error("encoding the config", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot encode the config")
		return
	}
	buf.WriteByte('\n')
	w.Header().Set("Content-Type", "application/json")
	w.Write(buf.Bytes())
}

// encodeConfig writes v as JSON that reads like the config file: fields
// are named by their yaml tags and keep their order, and durations, sizes,
// rates and prefixes are strings as the file spells them. encoding/json
// would use Go field names and nanoseconds. A field tagged redact is
// hidden (see config.Backend).
func encodeConfig(buf *bytes.Buffer, v reflect.Value) error {
	if k := v.Kind(); k == reflect.Pointer || k == reflect.Interface {
		if v.IsNil() {
			buf.WriteString("null")
			return nil
		}
		return encodeConfig(buf, v.Elem())
	}
	switch x := v.Interface().(type) {
	case time.Duration:
		return encodeValue(buf, x.String())
	case encoding.TextMarshaler:
		text, err := x.MarshalText()
		if err != nil {
			return err
		}
		return encodeValue(buf, string(text))
	}

	switch v.Kind() {
	case reflect.Struct:
		buf.WriteByte('{')
		first := true
		for i := range v.NumField() {
			f := v.Type().Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if !f.IsExported() || name == "-" {
				continue
			}
			if name == "" {
				name = strings.ToLower(f.Name) // yaml's default
			}
			if !first {
				buf.WriteByte(',')
			}
			first = false
			encodeValue(buf, name)
			buf.WriteByte(':')
			if err := encodeField(buf, v.Field(i), f.Tag.Get("redact")); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		buf.WriteByte('}')
	case reflect.Slice, reflect.Array:
		buf.WriteByte('[')
		for i := range v.Len() {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeConfig(buf, v.Index(i)); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case reflect.Map:
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(a.String(), b.String()) })
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			encodeValue(buf, k.String())
			buf.WriteByte(':')
			if err := encodeConfig(buf, v.MapIndex(k)); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
		buf.WriteByte('}')
	default:
		return encodeValue(buf, v.Interface())
	}
	return nil
}

// encodeField writes a struct field, hiding it as its redact tag says. An
// empty secret is shown as it is, so a reader can tell that none is set.
func encodeField(buf *bytes.Buffer, v reflect.Value, redact string) error {
	switch {
	case redact == "secret" && !v.IsZero():
		return encodeValue(buf, redacted)
	case redact == "url" && v.Kind() == reflect.String:
		u, err := url.Parse(v.String())
		if err != nil {
			return encodeValue(buf, redacted)
		}
		return encodeValue(buf, u.Redacted())
	}
	return encodeConfig(buf, v)
}

func encodeValue(buf *bytes.Buffer, v any) error {
	b, err := json.Marshal(v)
	buf.Write(b)
	return err
}
