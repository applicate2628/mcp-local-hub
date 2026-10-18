package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
)

func requireStopBatchV2Fields(object map[string]json.RawMessage, names ...string) error {
	for _, name := range names {
		value, present := object[name]
		if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("stop_batch v2 missing required field %s", name)
		}
	}
	return nil
}

// Typed JSON decoding accepts case-insensitive field aliases even with
// DisallowUnknownFields. Check the exact declared vocabulary first, without
// changing the separate required/null/omitempty rules or dynamic map keys.
func requireStopBatchV2ExactKeys(raw []byte, kind reflect.Type) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	for kind.Kind() == reflect.Pointer {
		kind = kind.Elem()
	}
	// These types encode a scalar or an opaque payload, not their Go fields.
	if kind == reflect.TypeOf(time.Time{}) || kind == reflect.TypeOf(json.RawMessage{}) {
		return nil
	}
	switch kind.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return err
		}
		fields := make(map[string]reflect.Type)
		var collect func(reflect.Type)
		collect = func(structure reflect.Type) {
			for i := 0; i < structure.NumField(); i++ {
				field := structure.Field(i)
				name := strings.Split(field.Tag.Get("json"), ",")[0]
				if name == "-" {
					continue
				}
				if field.Anonymous && name == "" && field.Type.Kind() == reflect.Struct {
					collect(field.Type)
					continue
				}
				if field.PkgPath != "" {
					continue
				}
				if name == "" {
					name = field.Name
				}
				fields[name] = field.Type
			}
		}
		collect(kind)
		for name, value := range object {
			field, known := fields[name]
			if !known {
				return fmt.Errorf("stop_batch v2 noncanonical or unknown JSON field %s", name)
			}
			if err := requireStopBatchV2ExactKeys(value, field); err != nil {
				return fmt.Errorf("stop_batch v2 field %s: %w", name, err)
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := requireStopBatchV2ExactKeys(value, kind.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		var values map[string]json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := requireStopBatchV2ExactKeys(value, kind.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func rejectStopBatchV2DuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok || seen[key] {
					return fmt.Errorf("stop_batch v2 duplicate or invalid JSON key")
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("stop_batch v2 invalid JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("stop_batch v2 trailing JSON input")
	}
	return nil
}
