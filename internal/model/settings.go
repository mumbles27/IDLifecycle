package model

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

type ScheduleSettings struct {
	Enabled         bool   `json:"enabled"`
	Kind            string `json:"kind"` // interval | daily | cron
	IntervalMinutes int    `json:"interval_minutes"`
	DailyTime       string `json:"daily_time"` // HH:MM
	CronExpr        string `json:"cron_expr"`
	Timezone        string `json:"timezone"`
}

type ApprovalSettings struct {
	Create  string `json:"create"`
	Update  string `json:"update"`
	Disable string `json:"disable"`
	Enable  string `json:"enable"`
	Delete  string `json:"delete"`
}

// For returns the approval mode ("manual" | "auto") for a change type.
func (a ApprovalSettings) For(changeType string) string {
	switch changeType {
	case ChangeCreate:
		return a.Create
	case ChangeUpdate:
		return a.Update
	case ChangeDisable:
		return a.Disable
	case ChangeEnable:
		return a.Enable
	case ChangeDelete:
		return a.Delete
	}
	return "manual"
}

type EngineSettings struct {
	DryRun               bool `json:"dry_run"`
	MaxDestructivePerRun int  `json:"max_destructive_per_run"`
	DeleteAfterDays      int  `json:"delete_after_days"`
	RemoveStaleGroups    bool `json:"remove_stale_groups"`
	EnforceOU            bool `json:"enforce_ou"`
}

type ADSettings struct {
	Server               string   `json:"server"`
	SearchBases          []string `json:"search_bases"`
	DefaultOU            string   `json:"default_ou"`
	DisabledOU           string   `json:"disabled_ou"`
	IDAttribute          string   `json:"id_attribute"`
	UPNSuffix            string   `json:"upn_suffix"`
	StripGroupsOnDisable bool     `json:"strip_groups_on_disable"`
}

type NamingSettings struct {
	SamTemplate   string `json:"sam_template"`
	UPNTemplate   string `json:"upn_template"`
	CNTemplate    string `json:"cn_template"`
	MaxCandidates int    `json:"max_candidates"`
}

type NewUserSettings struct {
	PasswordLength        int  `json:"password_length"`
	ChangePasswordAtLogon bool `json:"change_password_at_logon"`
	Enabled               bool `json:"enabled"`
}

type Settings struct {
	Schedule ScheduleSettings `json:"schedule"`
	Approval ApprovalSettings `json:"approval"`
	Engine   EngineSettings   `json:"engine"`
	AD       ADSettings       `json:"ad"`
	Naming   NamingSettings   `json:"naming"`
	NewUser  NewUserSettings  `json:"new_user"`
}

// DefaultSettings are deliberately conservative: dry-run on, everything manual.
func DefaultSettings() Settings {
	return Settings{
		Schedule: ScheduleSettings{Enabled: false, Kind: "interval", IntervalMinutes: 60, DailyTime: "02:00", CronExpr: "0 */4 * * *", Timezone: "UTC"},
		Approval: ApprovalSettings{Create: "manual", Update: "manual", Disable: "manual", Enable: "manual", Delete: "manual"},
		Engine:   EngineSettings{DryRun: true, MaxDestructivePerRun: 10, DeleteAfterDays: 30, RemoveStaleGroups: true},
		AD:       ADSettings{IDAttribute: "employeeID", StripGroupsOnDisable: true},
		Naming: NamingSettings{
			SamTemplate:   `{{ if and .FirstName .LastName }}{{ first .FirstName 1 | lower }}{{ .LastName | lower | alnum }}{{ end }}`,
			UPNTemplate:   `{{ if and .FirstName .LastName }}{{ .FirstName | lower | alnum }}.{{ .LastName | lower | alnum }}{{ end }}`,
			CNTemplate:    `{{ if and .FirstName .LastName }}{{ .FirstName }} {{ .LastName }}{{ end }}`,
			MaxCandidates: 9,
		},
		NewUser: NewUserSettings{PasswordLength: 20, ChangePasswordAtLogon: true, Enabled: true},
	}
}

// Validate normalizes and checks a settings document. It mutates s to fill blanks.
func (s *Settings) Validate() error {
	sc := &s.Schedule
	switch sc.Kind {
	case "interval":
		if sc.IntervalMinutes < 1 || sc.IntervalMinutes > 7*24*60 {
			return fmt.Errorf("schedule.interval_minutes must be between 1 and 10080")
		}
	case "daily":
		if _, err := time.Parse("15:04", sc.DailyTime); err != nil {
			return fmt.Errorf("schedule.daily_time must be HH:MM")
		}
	case "cron":
		if _, err := cron.ParseStandard(sc.CronExpr); err != nil {
			return fmt.Errorf("schedule.cron_expr invalid: %v", err)
		}
	default:
		return fmt.Errorf("schedule.kind must be interval, daily or cron")
	}
	if sc.Timezone == "" {
		sc.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(sc.Timezone); err != nil {
		return fmt.Errorf("schedule.timezone %q is not a known time zone", sc.Timezone)
	}

	for name, v := range map[string]string{
		"create": s.Approval.Create, "update": s.Approval.Update, "disable": s.Approval.Disable,
		"enable": s.Approval.Enable, "delete": s.Approval.Delete,
	} {
		if v != "manual" && v != "auto" {
			return fmt.Errorf("approval.%s must be manual or auto", name)
		}
	}

	if s.Engine.MaxDestructivePerRun < 0 {
		return fmt.Errorf("engine.max_destructive_per_run must be >= 0")
	}
	if s.Engine.DeleteAfterDays < 0 {
		return fmt.Errorf("engine.delete_after_days must be >= 0 (0 disables deletion)")
	}

	s.AD.IDAttribute = strings.TrimSpace(s.AD.IDAttribute)
	if s.AD.IDAttribute == "" {
		return fmt.Errorf("ad.id_attribute is required")
	}
	cleaned := make([]string, 0, len(s.AD.SearchBases))
	for _, b := range s.AD.SearchBases {
		if b = strings.TrimSpace(b); b != "" {
			cleaned = append(cleaned, b)
		}
	}
	s.AD.SearchBases = cleaned

	if s.Naming.MaxCandidates < 1 || s.Naming.MaxCandidates > 99 {
		return fmt.Errorf("naming.max_candidates must be between 1 and 99")
	}
	if strings.TrimSpace(s.Naming.SamTemplate) == "" || strings.TrimSpace(s.Naming.UPNTemplate) == "" || strings.TrimSpace(s.Naming.CNTemplate) == "" {
		return fmt.Errorf("naming templates must not be empty")
	}
	if s.NewUser.PasswordLength < 14 || s.NewUser.PasswordLength > 128 {
		return fmt.Errorf("new_user.password_length must be between 14 and 128")
	}
	return nil
}

// ReadyForLive reports why the settings are not sufficient for writing to AD ("" when ready).
func (s Settings) ReadyForLive() string {
	switch {
	case len(s.AD.SearchBases) == 0:
		return "ad.search_bases must list at least one search base"
	case s.AD.DefaultOU == "":
		return "ad.default_ou is required"
	case s.AD.UPNSuffix == "":
		return "ad.upn_suffix is required"
	}
	return ""
}
