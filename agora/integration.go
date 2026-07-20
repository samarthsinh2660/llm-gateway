package agora

import (
	"fmt"
	"os"

	"github.com/michaelquigley/df/dd"
)

func loadIntegrationFile(path string) (*IntegrationFile, error) {
	file := &IntegrationFile{}
	if err := dd.MergeYAMLFile(file, path); err != nil {
		return nil, fmt.Errorf("load agora integration file '%s': %w", path, err)
	}
	return file, nil
}

func mergeIntegrationFile(cfg *Config, file *IntegrationFile) {
	if cfg == nil || file == nil {
		return
	}

	if cfg.APIEndpoint == "" {
		cfg.APIEndpoint = file.APIEndpoint
	}
	if cfg.EnvRoot == "" {
		cfg.EnvRoot = file.EnvRoot
	}
	if file.Advertisement == nil {
		return
	}
	if cfg.Advertisement == nil {
		cfg.Advertisement = &AdvertisementConfig{}
	}
	if len(cfg.Advertisement.WorkgroupIDs) == 0 {
		cfg.Advertisement.WorkgroupIDs = append([]string(nil), file.Advertisement.WorkgroupIDs...)
	}
	if cfg.Advertisement.ContractID == "" {
		cfg.Advertisement.ContractID = file.Advertisement.ContractID
	}
}

// ResolveConfig expands environment variables and merges the integration file.
func ResolveConfig(cfg *Config) error {
	if cfg == nil || !cfg.Enabled {
		return nil
	}

	if err := expandStrings(cfg); err != nil {
		return err
	}
	if cfg.IntegrationFile != "" {
		file, err := loadIntegrationFile(cfg.IntegrationFile)
		if err != nil {
			return err
		}
		mergeIntegrationFile(cfg, file)
		if err := expandStrings(cfg); err != nil {
			return err
		}
	}

	return nil
}

// expandStrings resolves ${VAR} references in the config's string fields. a
// value written non-empty that resolves empty is a directed error — silently
// blanking it would hand the field to a default the operator did not choose.
func expandStrings(cfg *Config) error {
	expand := func(field string, value *string) error {
		if *value == "" {
			return nil
		}
		expanded := os.ExpandEnv(*value)
		if expanded == "" {
			return fmt.Errorf("agora.%s resolves empty (unset environment variable?)", field)
		}
		*value = expanded
		return nil
	}

	if err := expand("integration_file", &cfg.IntegrationFile); err != nil {
		return err
	}
	if err := expand("api_endpoint", &cfg.APIEndpoint); err != nil {
		return err
	}
	if err := expand("env_root", &cfg.EnvRoot); err != nil {
		return err
	}
	if err := expand("instance_name", &cfg.InstanceName); err != nil {
		return err
	}
	if err := expand("description", &cfg.Description); err != nil {
		return err
	}

	if cfg.Advertisement != nil {
		if err := expand("advertisement.contract_id", &cfg.Advertisement.ContractID); err != nil {
			return err
		}
		for i := range cfg.Advertisement.WorkgroupIDs {
			field := fmt.Sprintf("advertisement.workgroup_ids[%d]", i)
			if err := expand(field, &cfg.Advertisement.WorkgroupIDs[i]); err != nil {
				return err
			}
		}
		for i := range cfg.Advertisement.Capabilities {
			field := fmt.Sprintf("advertisement.capabilities[%d]", i)
			if err := expand(field, &cfg.Advertisement.Capabilities[i]); err != nil {
				return err
			}
		}
	}
	if cfg.Serve != nil {
		if err := expand("serve.tunnel", &cfg.Serve.Tunnel); err != nil {
			return err
		}
	}
	return nil
}
