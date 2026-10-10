package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/config"
	"github.com/parkerbrown98/gotalk-server/internal/livekit"
	"github.com/parkerbrown98/gotalk-server/internal/mail"
	"github.com/parkerbrown98/gotalk-server/internal/storage"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// Configurable provider sections. Each can be set in the config file or environment, or
// (when its enabling key is not set there) through the setup wizard and the instance
// settings API, which store it in the database.
const (
	SectionStorage = "storage"
	SectionMail    = "mail"
	SectionVoice   = "voice"
	SectionCORS    = "cors"
)

// ConfigSections lists the sections in wizard order.
var ConfigSections = []string{SectionStorage, SectionMail, SectionVoice, SectionCORS}

// Where a section's effective settings come from.
const (
	// SourceDefault means built-in defaults (possibly with some non-enabling keys from
	// config); the section can be changed through the wizard or settings API.
	SourceDefault = "default"
	// SourceConfig means the config file or environment sets the section; it is read-only
	// in the wizard and settings API.
	SourceConfig = "config"
	// SourceSettings means the settings were saved through the wizard or settings API.
	SourceSettings = "settings"
)

// sectionKeys are the enabling keys that put a section under config-file/environment control.
var sectionKeys = map[string]string{
	SectionStorage: "storage.driver",
	SectionMail:    "mail.driver",
	SectionVoice:   "voice.livekit_url",
	SectionCORS:    "server.cors_allowed_origins",
}

// SectionKey returns the config key that makes a section read-only when set.
func SectionKey(section string) string { return sectionKeys[section] }

// VoiceSettings are the LiveKit connection settings the wizard manages. An empty
// LiveKitURL disables voice.
type VoiceSettings struct {
	LiveKitURL       string `json:"livekit_url" doc:"LiveKit URL clients connect to (ws, wss, http or https); empty disables voice"`
	LiveKitAPIURL    string `json:"livekit_api_url,omitempty" doc:"How this server reaches LiveKit's API, if different (e.g. http://livekit:7880)"`
	LiveKitAPIKey    string `json:"livekit_api_key,omitempty"`
	LiveKitAPISecret string `json:"livekit_api_secret,omitempty" doc:"At least 32 characters; write-only, leave empty to keep the current value"`
}

func (v VoiceSettings) Redacted() VoiceSettings { v.LiveKitAPISecret = ""; return v }

func (v VoiceSettings) SecretsSet() []string {
	if v.LiveKitAPISecret != "" {
		return []string{"livekit_api_secret"}
	}
	return []string{}
}

func (v VoiceSettings) KeepSecrets(prev VoiceSettings) VoiceSettings {
	if v.LiveKitAPISecret == "" {
		v.LiveKitAPISecret = prev.LiveKitAPISecret
	}
	return v
}

func voiceSettingsOf(c config.Voice) VoiceSettings {
	return VoiceSettings{LiveKitURL: c.LiveKitURL, LiveKitAPIURL: c.LiveKitAPIURL, LiveKitAPIKey: c.LiveKitAPIKey, LiveKitAPISecret: c.LiveKitAPISecret}
}

func (v VoiceSettings) apply(c config.Voice) config.Voice {
	c.LiveKitURL, c.LiveKitAPIURL, c.LiveKitAPIKey, c.LiveKitAPISecret = v.LiveKitURL, v.LiveKitAPIURL, v.LiveKitAPIKey, v.LiveKitAPISecret
	return c
}

// CORSSettings controls which browser origins may call the API and open gateway
// connections.
type CORSSettings struct {
	AllowedOrigins   []string `json:"allowed_origins" doc:"Origins such as https://app.example.com, https://*.example.com, or * for any"`
	AllowCredentials bool     `json:"allow_credentials,omitempty" doc:"Allow credentialed requests (cannot be combined with *)"`
}

// ProviderSettings carries settings for any subset of sections; nil sections are left
// alone.
type ProviderSettings struct {
	Storage *storage.Settings `json:"storage,omitempty"`
	Mail    *mail.Settings    `json:"mail,omitempty"`
	Voice   *VoiceSettings    `json:"voice,omitempty"`
	CORS    *CORSSettings     `json:"cors,omitempty"`
}

func (p ProviderSettings) sections() []string {
	var out []string
	if p.Storage != nil {
		out = append(out, SectionStorage)
	}
	if p.Mail != nil {
		out = append(out, SectionMail)
	}
	if p.Voice != nil {
		out = append(out, SectionVoice)
	}
	if p.CORS != nil {
		out = append(out, SectionCORS)
	}
	return out
}

// Providers is the live set of pluggable backends, rebuilt whenever settings change.
type Providers struct {
	Revision int64

	Storage         storage.Backend
	StorageSettings storage.Settings
	StorageErr      error

	Mail         mail.Sender // nil when email is disabled or misconfigured
	MailSettings mail.Settings
	MailErr      error

	Voice         *livekit.Client // nil when voice is disabled or misconfigured
	VoiceSettings VoiceSettings
	VoiceErr      error

	CORS CORSSettings

	Sources map[string]string
	// Saved holds the rows stored in the database, by section.
	Saved map[string]store.InstanceConfig
}

// Providers returns the current backends.
func (s *Service) Providers() *Providers { return s.providers.Load() }

func (s *Service) storageBackend() storage.Backend { return s.Providers().Storage }

func (s *Service) mailer() mail.Sender { return s.Providers().Mail }

func (s *Service) voiceClient() *livekit.Client { return s.Providers().Voice }

// MailEnabled reports whether outgoing email works.
func (s *Service) MailEnabled() bool { return s.mailer() != nil }

// StorageEnabled reports whether a storage backend is available for uploads.
func (s *Service) StorageEnabled() bool { return s.storageBackend() != nil }

// effectiveSettings layers database rows over the loaded configuration: a section whose
// enabling key is set in the config file or environment ignores its row.
func (s *Service) effectiveSettings(rows map[string]store.InstanceConfig) (ProviderSettings, map[string]string, error) {
	cfg := s.cfg
	st := cfg.Storage
	ml := cfg.Mail
	vc := voiceSettingsOf(cfg.Voice)
	cors := CORSSettings{AllowedOrigins: slices.Clone(cfg.Server.CORSAllowedOrigins), AllowCredentials: cfg.Server.CORSAllowCredentials}
	targets := map[string]any{SectionStorage: &st, SectionMail: &ml, SectionVoice: &vc, SectionCORS: &cors}

	sources := map[string]string{}
	var errs []error
	for _, section := range ConfigSections {
		switch row, saved := rows[section]; {
		case cfg.IsSet(sectionKeys[section]):
			sources[section] = SourceConfig
		case saved:
			sources[section] = SourceSettings
			// Unmarshalling onto the config values layers the saved fields over them.
			if err := json.Unmarshal(row.Settings, targets[section]); err != nil {
				errs = append(errs, fmt.Errorf("stored %s settings are unreadable: %w", section, err))
			}
		default:
			sources[section] = SourceDefault
		}
	}
	return ProviderSettings{Storage: &st, Mail: &ml, Voice: &vc, CORS: &cors}, sources, errors.Join(errs...)
}

// buildProviders opens backends for settings, reusing prev's where settings are unchanged.
func (s *Service) buildProviders(eff ProviderSettings, prev *Providers) *Providers {
	p := &Providers{StorageSettings: *eff.Storage, MailSettings: *eff.Mail, VoiceSettings: *eff.Voice, CORS: *eff.CORS}

	if prev != nil && reflect.DeepEqual(prev.StorageSettings, p.StorageSettings) && prev.StorageErr == nil {
		p.Storage = prev.Storage
	} else {
		p.Storage, p.StorageErr = storage.Open(p.StorageSettings)
	}
	if prev != nil && reflect.DeepEqual(prev.MailSettings, p.MailSettings) && prev.MailErr == nil {
		p.Mail = prev.Mail
	} else {
		p.Mail, p.MailErr = mail.Open(p.MailSettings, mail.Env{Logger: s.log})
	}
	if prev != nil && prev.VoiceSettings == p.VoiceSettings && prev.VoiceErr == nil {
		p.Voice = prev.Voice
	} else {
		p.Voice, p.VoiceErr = openVoice(p.VoiceSettings)
	}
	return p
}

func openVoice(v VoiceSettings) (*livekit.Client, error) {
	if v.LiveKitURL == "" {
		return nil, nil
	}
	if err := config.ValidateVoice(v.apply(config.Voice{})); err != nil {
		return nil, err
	}
	return livekit.New(v.LiveKitURL, v.LiveKitAPIURL, v.LiveKitAPIKey, v.LiveKitAPISecret)
}

// ReloadProviders re-reads saved settings and rebuilds the backends that changed.
func (s *Service) ReloadProviders(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	rev, err := s.q.GetConfigRevision(ctx)
	if err != nil {
		return err
	}
	list, err := s.q.ListInstanceConfig(ctx)
	if err != nil {
		return err
	}
	rows := make(map[string]store.InstanceConfig, len(list))
	for _, r := range list {
		rows[r.Section] = r
	}
	eff, sources, effErr := s.effectiveSettings(rows)
	if effErr != nil {
		s.log.Error("ignoring unreadable saved settings", "error", effErr)
	}
	p := s.buildProviders(eff, s.providers.Load())
	p.Revision, p.Sources, p.Saved = rev, sources, rows
	for section, perr := range map[string]error{SectionStorage: p.StorageErr, SectionMail: p.MailErr, SectionVoice: p.VoiceErr} {
		if perr != nil {
			s.log.Error("provider is misconfigured", "section", section, "source", sources[section], "error", perr)
		}
	}
	s.providers.Store(p)
	s.health.reset()
	return nil
}

// RunConfigWatcher reloads providers when another replica changes saved settings.
func (s *Service) RunConfigWatcher(ctx context.Context) {
	t := time.NewTicker(configPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rev, err := s.q.GetConfigRevision(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("checking for settings changes", "error", err)
			}
			continue
		}
		if rev != s.Providers().Revision {
			if err := s.ReloadProviders(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("reloading settings", "error", err)
			}
		}
	}
}

// configPoll is how often replicas look for settings saved by another replica.
var configPoll = 15 * time.Second

// SectionView describes one section for administrators. Settings are redacted.
type SectionView struct {
	Section    string
	Source     string
	Editable   bool
	Key        string
	Settings   any
	SecretsSet []string
	UpdatedAt  *time.Time
	UpdatedBy  *uuid.UUID
	Error      string
}

// ConfigView describes every section's effective settings.
func (s *Service) ConfigView() []SectionView {
	p := s.Providers()
	out := make([]SectionView, 0, len(ConfigSections))
	for _, section := range ConfigSections {
		v := SectionView{Section: section, Source: p.Sources[section], Key: sectionKeys[section]}
		v.Editable = v.Source != SourceConfig
		switch section {
		case SectionStorage:
			v.Settings, v.SecretsSet = p.StorageSettings.Redacted(), p.StorageSettings.SecretsSet()
			v.Error = errString(p.StorageErr)
		case SectionMail:
			v.Settings, v.SecretsSet = p.MailSettings.Redacted(), p.MailSettings.SecretsSet()
			v.Error = errString(p.MailErr)
		case SectionVoice:
			v.Settings, v.SecretsSet = p.VoiceSettings.Redacted(), p.VoiceSettings.SecretsSet()
			v.Error = errString(p.VoiceErr)
		case SectionCORS:
			v.Settings, v.SecretsSet = p.CORS, []string{}
		}
		if row, ok := p.Saved[section]; ok && v.Source == SourceSettings {
			v.UpdatedAt, v.UpdatedBy = &row.UpdatedAt, row.UpdatedBy
		}
		out = append(out, v)
	}
	return out
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// candidate is a section's proposed settings with the backend built from them.
type candidate struct {
	section string
	data    []byte
	storage storage.Backend
	mail    mail.Sender
	voice   *livekit.Client
}

// prepareSettings validates the given sections against the current providers: secrets
// left empty keep their current values, and each section must be editable.
func (s *Service) prepareSettings(in ProviderSettings) ([]candidate, error) {
	p := s.Providers()
	var out []candidate
	for _, section := range in.sections() {
		if p.Sources[section] == SourceConfig {
			return nil, apperr.Conflict("%s settings are set by the config file or environment (%s); change them there",
				section, strings.ToUpper("GOTALK_"+strings.ReplaceAll(sectionKeys[section], ".", "_")))
		}
		c := candidate{section: section}
		var v any
		var err error
		switch section {
		case SectionStorage:
			st := in.Storage.KeepSecrets(p.StorageSettings)
			if st.Driver == "" {
				return nil, apperr.Invalid("storage.driver is required")
			}
			c.storage, err = storage.Open(st)
			v = st
		case SectionMail:
			ml := in.Mail.KeepSecrets(p.MailSettings)
			c.mail, err = mail.Open(ml, mail.Env{Logger: s.log})
			v = ml
		case SectionVoice:
			vc := in.Voice.KeepSecrets(p.VoiceSettings)
			if vc.LiveKitURL != "" && vc.LiveKitAPIKey == "" {
				vc.LiveKitAPIKey = p.VoiceSettings.LiveKitAPIKey
			}
			c.voice, err = openVoice(vc)
			v = vc
		case SectionCORS:
			cors := *in.CORS
			if len(cors.AllowedOrigins) == 0 {
				err = errors.New("cors.allowed_origins must list at least one origin (use * for any)")
			} else {
				err = config.ValidateCORS(cors.AllowedOrigins, cors.AllowCredentials)
			}
			v = cors
		}
		if err != nil {
			return nil, apperr.Invalid("%s: %s", section, err.Error())
		}
		if c.data, err = json.Marshal(v); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// checkCandidates runs live checks on prepared settings. testEmailTo, when set, also
// sends a test message through the candidate mail settings.
func (s *Service) checkCandidates(ctx context.Context, cands []candidate, testEmailTo string) []Check {
	var out []Check
	for _, c := range cands {
		switch c.section {
		case SectionStorage:
			out = append(out, checkStorage(ctx, c.storage, nil))
		case SectionMail:
			chk := checkMail(ctx, c.mail, nil)
			if chk.Status == CheckOK && testEmailTo != "" {
				chk = s.sendTestEmail(ctx, c.mail, testEmailTo)
			}
			out = append(out, chk)
		case SectionVoice:
			out = append(out, checkVoice(ctx, c.voice, nil))
		case SectionCORS:
			out = append(out, Check{Name: "cors", Status: CheckOK, Detail: "origins are valid"})
		}
	}
	return out
}

// TestSettings validates and live-checks settings without saving them.
func (s *Service) TestSettings(ctx context.Context, p *Principal, in ProviderSettings, testEmailTo string) ([]Check, error) {
	if !p.User.IsInstanceAdmin {
		return nil, apperr.Forbidden("only instance administrators can change instance settings")
	}
	return s.testSettings(ctx, in, testEmailTo)
}

func (s *Service) testSettings(ctx context.Context, in ProviderSettings, testEmailTo string) ([]Check, error) {
	if testEmailTo != "" {
		if err := validateEmail(testEmailTo); err != nil {
			return nil, err
		}
	}
	cands, err := s.prepareSettings(in)
	if err != nil {
		return nil, err
	}
	return s.checkCandidates(ctx, cands, testEmailTo), nil
}

// SaveSettings validates, live-checks and stores settings for the given sections, then
// applies them on every replica. Sections whose check fails are not saved unless force
// is set.
func (s *Service) SaveSettings(ctx context.Context, p *Principal, in ProviderSettings, force bool) ([]Check, error) {
	if !p.User.IsInstanceAdmin {
		return nil, apperr.Forbidden("only instance administrators can change instance settings")
	}
	if len(in.sections()) == 0 {
		return nil, apperr.Invalid("no settings to save")
	}
	cands, checks, err := s.prepareAndCheck(ctx, in, force)
	if err != nil {
		return checks, err
	}
	if err := s.tx(ctx, func(q *store.Queries) error { return s.storeCandidates(ctx, q, cands, &p.User.ID) }); err != nil {
		return checks, err
	}
	s.log.Info("instance settings changed", "sections", in.sections(), "by", p.User.Username)
	return checks, s.ReloadProviders(ctx)
}

func (s *Service) prepareAndCheck(ctx context.Context, in ProviderSettings, force bool) ([]candidate, []Check, error) {
	cands, err := s.prepareSettings(in)
	if err != nil {
		return nil, nil, err
	}
	checks := s.checkCandidates(ctx, cands, "")
	if !force {
		for _, c := range checks {
			if c.Status == CheckError {
				return nil, checks, apperr.Invalid("%s check failed: %s", c.Name, c.Detail)
			}
		}
	}
	return cands, checks, nil
}

func (s *Service) storeCandidates(ctx context.Context, q *store.Queries, cands []candidate, by *uuid.UUID) error {
	for _, c := range cands {
		if _, err := q.UpsertInstanceConfig(ctx, store.UpsertInstanceConfigParams{
			Section: c.section, Settings: c.data, UpdatedBy: by,
		}); err != nil {
			return err
		}
	}
	_, err := q.BumpConfigRevision(ctx)
	return err
}

// ResetSettings drops a section's saved settings, so the config file, environment and
// defaults apply again.
func (s *Service) ResetSettings(ctx context.Context, p *Principal, section string) error {
	if !p.User.IsInstanceAdmin {
		return apperr.Forbidden("only instance administrators can change instance settings")
	}
	if _, ok := sectionKeys[section]; !ok {
		return apperr.NotFound("unknown settings section %q", section)
	}
	err := s.tx(ctx, func(q *store.Queries) error {
		n, err := q.DeleteInstanceConfig(ctx, section)
		if err != nil || n == 0 {
			return err
		}
		_, err = q.BumpConfigRevision(ctx)
		return err
	})
	if err != nil {
		return err
	}
	s.log.Info("instance settings reset", "section", section, "by", p.User.Username)
	return s.ReloadProviders(ctx)
}

// ResetAllSettings drops every saved section (used by `gotalk setup --reset-settings`).
func (s *Service) ResetAllSettings(ctx context.Context) (int64, error) {
	var n int64
	err := s.tx(ctx, func(q *store.Queries) error {
		var err error
		if n, err = q.DeleteAllInstanceConfig(ctx); err != nil || n == 0 {
			return err
		}
		_, err = q.BumpConfigRevision(ctx)
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, s.ReloadProviders(ctx)
}
