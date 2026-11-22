package config

import (
	"reflect"
	"strings"
	"time"
)

// secretKeys are the config paths whose values never leave the process.
var secretKeys = map[string]bool{"auth.tokens": true, "firecrawl.api_key": true, "redis.password": true}

// Describe returns the effective configuration as a nested map for the debug UI:
// keys are the config key names, durations are rendered as strings ("30s") and
// secrets are replaced by "***" (or "(unset)" when empty).
func (c *Config) Describe() map[string]any {
	return describeStruct(reflect.ValueOf(*c), "")
}

func describeStruct(v reflect.Value, prefix string) map[string]any {
	out := make(map[string]any, v.NumField())
	t := v.Type()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("mapstructure"), ",")
		if name == "" || name == "-" {
			continue
		}
		path := prefix + name
		f := v.Field(i)
		switch {
		case secretKeys[path]:
			if f.IsZero() {
				out[name] = "(unset)"
			} else {
				out[name] = "***"
			}
		case f.Kind() == reflect.Struct:
			out[name] = describeStruct(f, path+".")
		case f.Type() == reflect.TypeOf(time.Duration(0)):
			out[name] = time.Duration(f.Int()).String()
		default:
			out[name] = f.Interface()
		}
	}
	return out
}
