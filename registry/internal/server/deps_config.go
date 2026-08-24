package server

import "sharedd/registry/internal/config"

// Алиасы конфиг-пакета: сервер и тесты исторически работают с короткими
// именами. Владелец формата TOML и валидации — internal/config.
type resolvedRegistryConfig = config.Resolved

func loadRegistryConfig() (*resolvedRegistryConfig, error) { return config.Load() }

type RegistryConfig = config.RegistryConfig

var applyRegistryDefaults = config.ApplyRegistryDefaults

var validateRegistryConfig = config.ValidateRegistryConfig
