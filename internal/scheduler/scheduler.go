package scheduler

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"sesame-bot/internal/crypto"
	"sesame-bot/internal/db"
	"sesame-bot/internal/models"
)

// These types mirror the ones in main package to build a config value.
// The actual runAction call happens via the provided ActionFunc.

type ActionFunc func(cfg interface{}, action string) error

type Scheduler struct {
	pool         *pgxpool.Pool
	encKey       []byte
	MemPasswords sync.Map // map[userID string] -> plaintext password
	runAction    func(userID, email, password string, headless, weekend bool,
		hoursIn, hoursOut, officeDays string,
		offLat, offLon, homeLat, homeLon float64,
		overrides []models.DayOverride,
		action string,
		scheduledAt time.Time,
	) error
}

// RunActionFn is the type of the function provided by main to do the actual browser automation.
type RunActionFn func(
	userID, email, password string,
	headless, weekend bool,
	hoursIn, hoursOut, officeDays string,
	offLat, offLon, homeLat, homeLon float64,
	overrides []models.DayOverride,
	action string,
	scheduledAt time.Time,
) error

func New(pool *pgxpool.Pool, encKey []byte, fn RunActionFn) *Scheduler {
	return &Scheduler{pool: pool, encKey: encKey, runAction: fn}
}

func (s *Scheduler) Run(ctx context.Context) {
	executed := make(map[string]map[string]bool)
	lastDate := ""

	log.Println("Scheduler multi-usuario iniciado")

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		now := time.Now()
		today := now.Format("2006-01-02")

		if today != lastDate {
			executed = make(map[string]map[string]bool)
			lastDate = today
			log.Printf("Scheduler: nuevo día %s", today)
		}

		users, err := db.LoadActiveUsersWithConfig(ctx, s.pool)
		if err != nil {
			log.Printf("Scheduler: error cargando usuarios: %v", err)
			time.Sleep(30 * time.Second)
			continue
		}

		for _, uw := range users {
			password := s.resolvePassword(uw)
			if password == "" {
				continue
			}

			schedule := buildSchedule(uw, now.Weekday())
			if len(schedule) == 0 {
				continue
			}

			uid := uw.User.ID
			if _, ok := executed[uid]; !ok {
				executed[uid] = make(map[string]bool)
			}

			for _, st := range schedule {
				key := fmt.Sprintf("%s-%02d:%02d-%s", today, st.hour, st.minute, st.action)
				if executed[uid][key] {
					continue
				}
				if now.Hour() == st.hour && now.Minute() == st.minute {
					executed[uid][key] = true
					c := uw.Config
					overrides := uw.DayOverrides
					action := st.action
					scheduledAt := now
					go func(uid, email, pw string) {
						err := s.runAction(
							uid, email, pw,
							c.Headless, c.Weekend,
							c.HoursIn, c.HoursOut, c.OfficeDays,
							c.LocationOfficeLat, c.LocationOfficeLon,
							c.LocationHomeLat, c.LocationHomeLon,
							overrides, action, scheduledAt,
						)
						status, msg := "ok", ""
						if err != nil {
							status, msg = "error", err.Error()
							log.Printf("Scheduler [%s]: error %s: %v", uid, action, err)
						} else {
							log.Printf("Scheduler [%s]: %s completado", uid, action)
						}
						dbCtx := context.Background()
						if logErr := db.InsertCheckinLog(dbCtx, s.pool, uid, action, status, msg, scheduledAt); logErr != nil {
							log.Printf("Scheduler: error guardando log: %v", logErr)
						}
					}(uid, c.SesameEmail, password)
				}
			}
		}

		time.Sleep(30 * time.Second)
	}
}

func (s *Scheduler) resolvePassword(uw models.UserWithConfig) string {
	// Memory-only password takes priority
	if pw, ok := s.MemPasswords.Load(uw.User.ID); ok {
		return pw.(string)
	}
	// Encrypted password from DB
	if uw.Config.SesamePasswordEnc != "" && len(s.encKey) > 0 {
		pw, err := crypto.Decrypt(s.encKey, uw.Config.SesamePasswordEnc)
		if err != nil {
			log.Printf("Scheduler [%s]: error descifrando password: %v", uw.User.ID, err)
			return ""
		}
		return pw
	}
	return ""
}

type scheduledEntry struct {
	hour   int
	minute int
	action string
}

func buildSchedule(uw models.UserWithConfig, day time.Weekday) []scheduledEntry {
	isWeekend := day == time.Saturday || day == time.Sunday
	if isWeekend && !uw.Config.Weekend {
		return nil
	}

	inTimes := splitCSV(uw.Config.HoursIn)
	outTimes := splitCSV(uw.Config.HoursOut)

	// Apply day overrides
	for _, o := range uw.DayOverrides {
		if time.Weekday(o.Weekday) == day {
			if o.HoursIn != "" {
				inTimes = splitCSV(o.HoursIn)
			}
			if o.HoursOut != "" {
				outTimes = splitCSV(o.HoursOut)
			}
		}
	}

	var entries []scheduledEntry
	for _, raw := range inTimes {
		if h, m, ok := parseHHMM(raw); ok {
			entries = append(entries, scheduledEntry{h, m, "IN"})
		}
	}
	for _, raw := range outTimes {
		if h, m, ok := parseHHMM(raw); ok {
			entries = append(entries, scheduledEntry{h, m, "OUT"})
		}
	}
	return entries
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			part := s[start:i]
			if part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}

func parseHHMM(raw string) (int, int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(raw, "%d:%d", &h, &m); err != nil {
		return 0, 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}
