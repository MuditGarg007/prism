package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// Load reads, parses and validates the config file at path. Relative file
// paths inside it are resolved against the file's directory.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes data strictly (unknown keys are errors), applies defaults,
// resolves relative paths against baseDir and validates the result.
func Parse(data []byte, baseDir string) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config is empty")
		}
		return nil, err
	}
	cfg.applyDefaults()
	cfg.resolvePaths(baseDir)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) resolvePaths(baseDir string) {
	if baseDir == "" {
		return
	}
	resolve := func(p *string) {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(baseDir, *p)
		}
	}
	for _, l := range c.Listeners {
		if l.TLS == nil {
			continue
		}
		for i := range l.TLS.Certs {
			resolve(&l.TLS.Certs[i].Cert)
			resolve(&l.TLS.Certs[i].Key)
		}
	}
	for _, u := range c.Upstreams {
		if u == nil {
			continue
		}
		for _, b := range u.Backends {
			if b.TLS != nil {
				resolve(&b.TLS.CAFile)
			}
		}
	}
}
