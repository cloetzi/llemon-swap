package config

import "time"

const (
	ProviderTypeLemonade = "lemonade"

	ResidencyHardPinned = "hard-pinned"
	ResidencyPreferred  = "preferred"
	ResidencyTransient  = "transient"
	ResidencyExternal   = "external"
)

// ProviderConfig describes a long-lived inference provider. Credentials are
// named by environment variable so they never need to appear in YAML.
type ProviderConfig struct {
	Type                string        `yaml:"type" json:"type"`
	BaseURL             string        `yaml:"baseURL" json:"baseURL"`
	APIKeyEnv           string        `yaml:"apiKeyEnv" json:"apiKeyEnv,omitempty"`
	AdminAPIKeyEnv      string        `yaml:"adminApiKeyEnv" json:"adminApiKeyEnv,omitempty"`
	ManagementTimeout   time.Duration `yaml:"managementTimeout" json:"managementTimeout"`
	ColdStartTimeout    time.Duration `yaml:"coldStartTimeout" json:"coldStartTimeout"`
	DiscoveryInterval   time.Duration `yaml:"discoveryInterval" json:"discoveryInterval"`
	InsecureSkipVerify  bool          `yaml:"insecureSkipVerify" json:"insecureSkipVerify"`
	Required            bool          `yaml:"required" json:"required"`
	ResolvedAPIKey      string        `yaml:"-" json:"-"`
	ResolvedAdminAPIKey string        `yaml:"-" json:"-"`
}

func (c *ProviderConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type raw ProviderConfig
	defaults := raw{
		Type:              ProviderTypeLemonade,
		ManagementTimeout: 3 * time.Minute,
		ColdStartTimeout:  10 * time.Minute,
		DiscoveryInterval: 5 * time.Second,
		Required:          true,
	}
	if err := unmarshal(&defaults); err != nil {
		return err
	}
	*c = ProviderConfig(defaults)
	return nil
}

// LifecyclePoolConfig is deliberately separate from the existing matrix DSL.
// A lifecycle pool models capacity on one external provider; matrix continues
// to describe valid concurrent combinations for process-managed models.
type LifecyclePoolConfig struct {
	Provider         string        `yaml:"provider" json:"provider"`
	Capacity         int           `yaml:"capacity" json:"capacity"`
	RestorePreferred bool          `yaml:"restorePreferred" json:"restorePreferred"`
	TransientIdleTTL time.Duration `yaml:"transientIdleTTL" json:"transientIdleTTL"`
	ResidentFirst    bool          `yaml:"residentFirst" json:"residentFirst"`
	MaxResidentBurst int           `yaml:"maxResidentBurst" json:"maxResidentBurst"`
	MaxResidentWait  time.Duration `yaml:"maxResidentWait" json:"maxResidentWait"`
}

func (c *LifecyclePoolConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type raw LifecyclePoolConfig
	defaults := raw{
		Capacity:         1,
		RestorePreferred: true,
		ResidentFirst:    true,
		MaxResidentBurst: 8,
		MaxResidentWait:  10 * time.Second,
	}
	if err := unmarshal(&defaults); err != nil {
		return err
	}
	*c = LifecyclePoolConfig(defaults)
	return nil
}
