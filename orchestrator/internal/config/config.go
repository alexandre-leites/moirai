package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	DatabaseURL string
	GRPCBind    string
}

func Load() (Config, error) {
	databaseURL, err := value("LOOP_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	bind := os.Getenv("LOOP_GRPC_BIND")
	if bind == "" {
		bind = "0.0.0.0:50051"
	}
	if _, _, err := net.SplitHostPort(bind); err != nil {
		return Config{}, fmt.Errorf("LOOP_GRPC_BIND must be host:port: %w", err)
	}
	return Config{DatabaseURL: normalizeDatabaseURL(databaseURL), GRPCBind: bind}, nil
}

func value(name string) (string, error) {
	file := os.Getenv(name + "_FILE")
	plain := os.Getenv(name)
	if file != "" && plain != "" {
		return "", fmt.Errorf("%s and %s_FILE cannot both be set", name, name)
	}
	if file != "" {
		contents, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read %s_FILE: %w", name, err)
		}
		plain = strings.TrimSpace(string(contents))
	}
	if plain == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return plain, nil
}

func normalizeDatabaseURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return value
	}
	if parsed.Scheme == "postgresql+asyncpg" {
		parsed.Scheme = "postgresql"
	}
	return parsed.String()
}
