package notifier

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-openapi/strfmt"

	"github.com/grafana/grafana/pkg/services/ngalert/api/tooling/definitions"
	"github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/services/ngalert/store"
	"github.com/grafana/grafana/pkg/util"
)

type UnknownReceiverError struct {
	UID string
}

func (e UnknownReceiverError) Error() string {
	return fmt.Sprintf("unknown receiver: %s", e.UID)
}

type AlertmanagerConfigRejectedError struct {
	Inner error
}

func (e AlertmanagerConfigRejectedError) Error() string {
	return fmt.Sprintf("failed to save and apply Alertmanager configuration: %s", e.Inner.Error())
}

type configurationStore interface {
	GetLatestAlertmanagerConfiguration(ctx context.Context, orgID int64) (*models.AlertConfiguration, error)
}

func (moa *MultiOrgAlertmanager) GetAlertmanagerConfiguration(ctx context.Context, org int64) (definitions.GettableUserConfig, error) {
	amConfig, err := moa.configStore.GetLatestAlertmanagerConfiguration(ctx, org)
	if err != nil {
		return definitions.GettableUserConfig{}, fmt.Errorf("failed to get latest configuration: %w", err)
	}

	return moa.gettableUserConfigFromAMConfigString(ctx, org, amConfig.AlertmanagerConfiguration)
}

// ActivateHistoricalConfiguration will set the current alertmanager configuration to a previous value based on the provided
// alert_configuration_history id.
func (moa *MultiOrgAlertmanager) ActivateHistoricalConfiguration(ctx context.Context, orgId int64, id int64) error {
	config, err := moa.configStore.GetHistoricalConfiguration(ctx, orgId, id)
	if err != nil {
		return fmt.Errorf("failed to get historical alertmanager configuration: %w", err)
	}

	cfg, err := Load([]byte(config.AlertmanagerConfiguration))
	if err != nil {
		return fmt.Errorf("failed to unmarshal historical alertmanager configuration: %w", err)
	}

	am, err := moa.AlertmanagerFor(orgId)
	if err != nil {
		// It's okay if the alertmanager isn't ready yet, we're changing its config anyway.
		if !errors.Is(err, ErrAlertmanagerNotReady) {
			return err
		}
	}

	if err := am.SaveAndApplyConfig(ctx, cfg); err != nil {
		moa.logger.Error("Unable to save and apply historical alertmanager configuration", "error", err, "org", orgId, "id", id)
		return AlertmanagerConfigRejectedError{err}
	}
	moa.logger.Info("Applied historical alertmanager configuration", "org", orgId, "id", id)

	return nil
}

// GetAppliedAlertmanagerConfigurations returns the last n configurations marked as applied for a given org.
func (moa *MultiOrgAlertmanager) GetAppliedAlertmanagerConfigurations(ctx context.Context, org int64, limit int) ([]*definitions.GettableHistoricUserConfig, error) {
	configs, err := moa.configStore.GetAppliedConfigurations(ctx, org, limit)
	if err != nil {
		return []*definitions.GettableHistoricUserConfig{}, fmt.Errorf("failed to get applied configurations: %w", err)
	}

	gettableHistoricConfigs := make([]*definitions.GettableHistoricUserConfig, 0, len(configs))
	for _, config := range configs {
		appliedAt := strfmt.DateTime(time.Unix(config.LastApplied, 0).UTC())
		gettableConfig, err := moa.gettableUserConfigFromAMConfigString(ctx, org, config.AlertmanagerConfiguration)
		if err != nil {
			// If there are invalid records, skip them and return the valid ones.
			moa.logger.Warn("Invalid configuration found in alert configuration history table", "id", config.ID, "orgID", org)
			continue
		}

		gettableHistoricConfig := definitions.GettableHistoricUserConfig{
			ID:                      config.ID,
			TemplateFiles:           gettableConfig.TemplateFiles,
			TemplateFileProvenances: gettableConfig.TemplateFileProvenances,
			AlertmanagerConfig:      gettableConfig.AlertmanagerConfig,
			LastApplied:             &appliedAt,
		}
		gettableHistoricConfigs = append(gettableHistoricConfigs, &gettableHistoricConfig)
	}

	return gettableHistoricConfigs, nil
}

func (moa *MultiOrgAlertmanager) gettableUserConfigFromAMConfigString(ctx context.Context, orgID int64, config string) (definitions.GettableUserConfig, error) {
	cfg, err := Load([]byte(config))
	if err != nil {
		return definitions.GettableUserConfig{}, fmt.Errorf("failed to unmarshal alertmanager configuration: %w", err)
	}
	result := definitions.GettableUserConfig{
		TemplateFiles: cfg.TemplateFiles,
		AlertmanagerConfig: definitions.GettableApiAlertingConfig{
			Config: cfg.AlertmanagerConfig.Config,
		},
	}
	for _, recv := range cfg.AlertmanagerConfig.Receivers {
		receivers := make([]*definitions.GettableGrafanaReceiver, 0, len(recv.PostableGrafanaReceivers.GrafanaManagedReceivers))
		for _, pr := range recv.PostableGrafanaReceivers.GrafanaManagedReceivers {
			secureFields := make(map[string]bool, len(pr.SecureSettings))
			for k := range pr.SecureSettings {
				decryptedValue, err := moa.Crypto.getDecryptedSecret(pr, k)
				if err != nil {
					return definitions.GettableUserConfig{}, fmt.Errorf("failed to decrypt stored secure setting: %w", err)
				}
				if decryptedValue == "" {
					continue
				}
				secureFields[k] = true
			}
			gr := definitions.GettableGrafanaReceiver{
				UID:                   pr.UID,
				Name:                  pr.Name,
				Type:                  pr.Type,
				DisableResolveMessage: pr.DisableResolveMessage,
				Settings:              pr.Settings,
				SecureFields:          secureFields,
			}
			receivers = append(receivers, &gr)
		}
		gettableApiReceiver := definitions.GettableApiReceiver{
			GettableGrafanaReceivers: definitions.GettableGrafanaReceivers{
				GrafanaManagedReceivers: receivers,
			},
		}
		gettableApiReceiver.Name = recv.Name
		result.AlertmanagerConfig.Receivers = append(result.AlertmanagerConfig.Receivers, &gettableApiReceiver)
	}

	result, err = moa.mergeProvenance(ctx, result, orgID)
	if err != nil {
		return definitions.GettableUserConfig{}, err
	}

	return result, nil
}

func (moa *MultiOrgAlertmanager) ApplyAlertmanagerConfiguration(ctx context.Context, org int64, config definitions.PostableUserConfig) error {
	// We cannot add this validation to PostableUserConfig as that struct is used for both
	// Grafana Alertmanager (where inhibition rules are not supported) and External Alertmanagers
	// (including Mimir) where inhibition rules are supported.
	if len(config.AlertmanagerConfig.InhibitRules) > 0 {
		return errors.New("inhibition rules are not supported")
	}

	// Get the last known working configuration
	_, err := moa.configStore.GetLatestAlertmanagerConfiguration(ctx, org)
	if err != nil {
		// If we don't have a configuration there's nothing for us to know and we should just continue saving the new one
		if !errors.Is(err, store.ErrNoAlertmanagerConfiguration) {
			return fmt.Errorf("failed to get latest configuration %w", err)
		}
	}

	if err := moa.Crypto.ProcessSecureSettings(ctx, org, config.AlertmanagerConfig.Receivers); err != nil {
		return fmt.Errorf("failed to post process Alertmanager configuration: %w", err)
	}

	if err := assignReceiverConfigsUIDs(config.AlertmanagerConfig.Receivers); err != nil {
		return fmt.Errorf("failed to assign missing uids: %w", err)
	}

	am, err := moa.AlertmanagerFor(org)
	if err != nil {
		// It's okay if the alertmanager isn't ready yet, we're changing its config anyway.
		if !errors.Is(err, ErrAlertmanagerNotReady) {
			return err
		}
	}

	if err := am.SaveAndApplyConfig(ctx, &config); err != nil {
		moa.logger.Error("Unable to save and apply alertmanager configuration", "error", err)
		return AlertmanagerConfigRejectedError{err}
	}

	return nil
}

// assignReceiverConfigsUIDs assigns missing UUIDs to receiver configs.
func assignReceiverConfigsUIDs(c []*definitions.PostableApiReceiver) error {
	seenUIDs := make(map[string]struct{})
	// encrypt secure settings for storing them in DB
	for _, r := range c {
		switch r.Type() {
		case definitions.GrafanaReceiverType:
			for _, gr := range r.PostableGrafanaReceivers.GrafanaManagedReceivers {
				if gr.UID == "" {
					retries := 5
					for i := 0; i < retries; i++ {
						gen := util.GenerateShortUID()
						_, ok := seenUIDs[gen]
						if !ok {
							gr.UID = gen
							break
						}
					}
					if gr.UID == "" {
						return fmt.Errorf("all %d attempts to generate UID for receiver have failed; please retry", retries)
					}
				}
				seenUIDs[gr.UID] = struct{}{}
			}
		default:
		}
	}
	return nil
}

type provisioningStore interface {
	GetProvenance(ctx context.Context, o models.Provisionable, org int64) (models.Provenance, error)
	GetProvenances(ctx context.Context, org int64, resourceType string) (map[string]models.Provenance, error)
	SetProvenance(ctx context.Context, o models.Provisionable, org int64, p models.Provenance) error
	DeleteProvenance(ctx context.Context, o models.Provisionable, org int64) error
}

func (moa *MultiOrgAlertmanager) mergeProvenance(ctx context.Context, config definitions.GettableUserConfig, org int64) (definitions.GettableUserConfig, error) {
	if config.AlertmanagerConfig.Route != nil {
		provenance, err := moa.ProvStore.GetProvenance(ctx, config.AlertmanagerConfig.Route, org)
		if err != nil {
			return definitions.GettableUserConfig{}, err
		}
		config.AlertmanagerConfig.Route.Provenance = definitions.Provenance(provenance)
	}

	cp := definitions.EmbeddedContactPoint{}
	cpProvs, err := moa.ProvStore.GetProvenances(ctx, org, cp.ResourceType())
	if err != nil {
		return definitions.GettableUserConfig{}, err
	}
	for _, receiver := range config.AlertmanagerConfig.Receivers {
		for _, contactPoint := range receiver.GrafanaManagedReceivers {
			if provenance, exists := cpProvs[contactPoint.UID]; exists {
				contactPoint.Provenance = definitions.Provenance(provenance)
			}
		}
	}

	tmpl := definitions.NotificationTemplate{}
	tmplProvs, err := moa.ProvStore.GetProvenances(ctx, org, tmpl.ResourceType())
	if err != nil {
		return definitions.GettableUserConfig{}, nil
	}
	config.TemplateFileProvenances = make(map[string]definitions.Provenance, len(tmplProvs))
	for key, provenance := range tmplProvs {
		config.TemplateFileProvenances[key] = definitions.Provenance(provenance)
	}

	mt := definitions.MuteTimeInterval{}
	mtProvs, err := moa.ProvStore.GetProvenances(ctx, org, mt.ResourceType())
	if err != nil {
		return definitions.GettableUserConfig{}, nil
	}
	config.AlertmanagerConfig.MuteTimeProvenances = make(map[string]definitions.Provenance, len(mtProvs))
	for key, provenance := range mtProvs {
		config.AlertmanagerConfig.MuteTimeProvenances[key] = definitions.Provenance(provenance)
	}

	return config, nil
}
