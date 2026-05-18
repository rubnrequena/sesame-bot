package models

import "time"

type User struct {
	ID           string
	Email        string
	PasswordHash string
	IsAdmin      bool
	IsActive     bool
	IsApproved   bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type UserConfig struct {
	ID                string
	UserID            string
	SesameEmail       string
	SesamePasswordEnc string // AES-256-GCM ciphertext, empty = solo en memoria
	LocationOfficeLat float64
	LocationOfficeLon float64
	LocationHomeLat   float64
	LocationHomeLon   float64
	OfficeDays        string // "Tuesday,Thursday"
	WhatsappNumber    string // número de teléfono para notificaciones (ej: 5491112345678)
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type DayOverride struct {
	ID       string
	UserID   string
	Weekday  int // time.Weekday value
	HoursIn  string
	HoursOut string
}

type CheckinLog struct {
	ID          string
	UserID      string
	Action      string // "IN" or "OUT"
	Status      string // "ok", "error", "skipped"
	Message     string
	ScheduledAt time.Time
	ExecutedAt  time.Time
}

type Session struct {
	Token     string
	UserID    string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// UserWithConfig is loaded by the scheduler each tick
type UserWithConfig struct {
	User        User
	Config      UserConfig
	DayOverrides []DayOverride
}
