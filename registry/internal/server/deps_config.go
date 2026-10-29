package server

import "sharedd/registry/internal/config"

// Алиасы конфиг-пакета: сервер и тесты исторически работают с короткими
// именами. Владелец формата TOML и валидации — internal/config.
type resolvedRegistryConfig = config.Resolved

func loadRegistryConfig() (*resolvedRegistryConfig, error) { return config.Load() }

type RegistryConfig = config.RegistryConfig

var applyRegistryDefaults = config.ApplyRegistryDefaults

var validateRegistryConfig = config.ValidateRegistryConfig

// Мульти-мастер ([selection] masters_per_domain): потолок и резолвер.
const maxMastersPerDomain = config.MaxMastersPerDomain

var resolveMastersPerDomain = config.ResolveMastersPerDomain

// Профили установки: эффективное значение user_profiles_enabled.
var resolveUserProfilesEnabled = config.ResolveUserProfilesEnabled
