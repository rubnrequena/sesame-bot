package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"sesame-bot/internal/crypto"
	"sesame-bot/internal/db"
	"sesame-bot/internal/models"
	"sesame-bot/internal/ws"
)

// ErrSkipped signals that a check-in was intentionally skipped (e.g. public holiday).
var ErrSkipped = errors.New("skipped")

// These types mirror the ones in main package to build a config value.
// The actual runAction call happens via the provided ActionFunc.

type ActionFunc func(cfg interface{}, action string) error

type Scheduler struct {
	pool         *pgxpool.Pool
	encKey       []byte
	MemPasswords sync.Map // map[userID string] -> plaintext password
	runAction    RunActionFn
	wsClient     *ws.Whatsapp

	// Estado de jitter compartido entre el loop Run y las notificaciones
	// disparadas desde la web, para que ambas vean los mismos offsets ya
	// sorteados del día. Protegido por schedMu.
	schedMu       sync.Mutex
	jitterOffsets map[string]int
	jitterBags    map[string]*jitterDay
}

// RunActionFn is the type of the function provided by main to do the actual browser automation.
// It returns a location label ("Oficina" or "Casa") and an error.
type RunActionFn func(
	userID, email, password string,
	officeDays string,
	offLat, offLon, homeLat, homeLon float64,
	action string,
	scheduledAt time.Time,
) (string, error)

func New(pool *pgxpool.Pool, encKey []byte, fn RunActionFn, wsClient *ws.Whatsapp) *Scheduler {
	return &Scheduler{
		pool:          pool,
		encKey:        encKey,
		runAction:     fn,
		wsClient:      wsClient,
		jitterOffsets: make(map[string]int),
		jitterBags:    make(map[string]*jitterDay),
	}
}

func (s *Scheduler) Run(ctx context.Context) {
	executed := make(map[string]map[string]bool)
	// Users already sent the morning reminder today. Cleared on day change.
	morningNotified := make(map[string]bool)
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
			s.schedMu.Lock()
			s.jitterOffsets = make(map[string]int)
			s.jitterBags = make(map[string]*jitterDay)
			s.schedMu.Unlock()
			morningNotified = make(map[string]bool)
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

			s.schedMu.Lock()
			schedule := buildSchedule(uw, now, s.jitterOffsets, s.jitterBags)
			s.schedMu.Unlock()
			if len(schedule) == 0 {
				continue
			}

			uid := uw.User.ID
			if _, ok := executed[uid]; !ok {
				executed[uid] = make(map[string]bool)
			}

			// Morning reminder: one hour before the first IN of the day. Skipped on
			// days without an IN or when the target falls before midnight.
			if firstIN, ok := firstEntryOfType(schedule, "IN"); ok {
				earliest := firstIN.hour*60 + firstIN.minute
				notifyMin := earliest - 60
				if notifyMin >= 0 && now.Hour() == notifyMin/60 && now.Minute() == notifyMin%60 && !morningNotified[uid] {
					morningNotified[uid] = true
					locLabel := locationLabelForDay(uw.Config.OfficeDays, now.Weekday())
					whatsappNumber := uw.Config.WhatsappNumber
					go s.sendMorningReminder(uid, whatsappNumber, locLabel, schedule)
				}
			}

			for _, st := range schedule {
				key := fmt.Sprintf("%s-%02d:%02d-%s", today, st.hour, st.minute, st.action)
				if executed[uid][key] {
					continue
				}
				if now.Hour() == st.hour && now.Minute() == st.minute {
					executed[uid][key] = true
					c := uw.Config
					action := st.action
					scheduledAt := now
					go func(uid, email, pw, whatsappNumber string) {
						locLabel, err := s.runAction(
							uid, email, pw,
							c.OfficeDays,
							c.LocationOfficeLat, c.LocationOfficeLon,
							c.LocationHomeLat, c.LocationHomeLon,
							action, scheduledAt,
						)
						status, msg := "ok", locLabel
						if err != nil {
							if errors.Is(err, ErrSkipped) {
								status, msg = "skipped", err.Error()
								log.Printf("Scheduler [%s]: %s omitido: %s", uid, action, msg)
							} else {
								status, msg = "error", err.Error()
								log.Printf("Scheduler [%s]: error %s: %v", uid, action, err)
							}
						} else {
							log.Printf("Scheduler [%s]: %s completado en %s", uid, action, locLabel)
						}
						dbCtx := context.Background()
						if logErr := db.InsertCheckinLog(dbCtx, s.pool, uid, action, status, msg, scheduledAt); logErr != nil {
							log.Printf("Scheduler: error guardando log: %v", logErr)
						}
						s.sendWhatsappNotification(uid, action, status, locLabel, msg, whatsappNumber)
					}(uid, c.SesameEmail, password, c.WhatsappNumber)
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

// buildSchedule returns the scheduled entries for the given day using only day_overrides.
// Days without an override are inactive (return nil). Each entry has its configured
// jitter applied, drawing offsets without replacement from the user's day bag.
func buildSchedule(uw models.UserWithConfig, now time.Time, jitterOffsets map[string]int, jitterBags map[string]*jitterDay) []scheduledEntry {
	date := now.Format("2006-01-02")
	for _, o := range uw.DayOverrides {
		if time.Weekday(o.Weekday) != now.Weekday() {
			continue
		}
		var entries []scheduledEntry
		idx := 0
		for _, raw := range splitCSV(o.HoursIn) {
			h, m, ok := parseHHMM(raw)
			if !ok {
				continue
			}
			jh, jm := jitterTime(uw.User.ID, date, "IN", idx, h, m, o.JitterMinutes, jitterOffsets, jitterBags)
			entries = append(entries, scheduledEntry{jh, jm, "IN"})
			idx++
		}
		for _, raw := range splitCSV(o.HoursOut) {
			h, m, ok := parseHHMM(raw)
			if !ok {
				continue
			}
			jh, jm := jitterTime(uw.User.ID, date, "OUT", idx, h, m, o.JitterMinutes, jitterOffsets, jitterBags)
			entries = append(entries, scheduledEntry{jh, jm, "OUT"})
			idx++
		}
		return entries
	}
	return nil // día sin override = inactivo
}

// jitterDay is the per-user, per-day without-replacement bag of jitter offsets.
type jitterDay struct {
	jitter    int   // range the bag was filled for
	remaining []int // offsets not drawn yet
}

// jitterTime shifts baseHour:baseMinute by a random offset in [-jitter, +jitter]
// minutes, clamped to the same day. Offsets are drawn without replacement from the
// per-user, per-day bag (keyed by userID|date), so no offset repeats within the day
// until the bag is exhausted; IN and OUT share the same bag. The chosen offset is
// cached in offsets so it stays stable across scheduler ticks; a restart re-rolls.
func jitterTime(userID, date, action string, idx, baseHour, baseMinute, jitter int, offsets map[string]int, bags map[string]*jitterDay) (int, int) {
	if jitter <= 0 {
		return baseHour, baseMinute
	}
	key := fmt.Sprintf("%s|%s|%s|%d|%02d:%02d|%d", userID, date, action, idx, baseHour, baseMinute, jitter)
	offset, cached := offsets[key]
	if !cached {
		bagKey := userID + "|" + date
		bag, ok := bags[bagKey]
		if !ok {
			bag = &jitterDay{}
			bags[bagKey] = bag
		}
		offset = drawJitterOffset(bag, jitter, userID)
		offsets[key] = offset

		hour, minute := clampToDay(baseHour*60 + baseMinute + offset)
		log.Printf("Scheduler [%s]: %s base %02d:%02d ±%dmin -> %02d:%02d (offset %+d, quedan %d/%d)",
			userID, action, baseHour, baseMinute, jitter, hour, minute, offset, len(bag.remaining), 2*jitter+1)
		return hour, minute
	}
	return clampToDay(baseHour*60 + baseMinute + offset)
}

// drawJitterOffset removes and returns a random offset in [-jitter, +jitter] that
// this user has not used yet today. The bag is refilled when it runs out (new
// round) or when the configured range changes.
func drawJitterOffset(bag *jitterDay, jitter int, userID string) int {
	if bag.jitter != jitter || len(bag.remaining) == 0 {
		if bag.jitter == jitter {
			log.Printf("Scheduler [%s]: bolsa de jitter agotada, nueva ronda (%d valores)", userID, 2*jitter+1)
		}
		bag.jitter = jitter
		bag.remaining = allOffsets(jitter)
	}
	i := rand.IntN(len(bag.remaining))
	offset := bag.remaining[i]
	bag.remaining = append(bag.remaining[:i], bag.remaining[i+1:]...)
	return offset
}

// allOffsets returns every whole-minute offset in [-jitter, +jitter].
func allOffsets(jitter int) []int {
	offsets := make([]int, 0, 2*jitter+1)
	for o := -jitter; o <= jitter; o++ {
		offsets = append(offsets, o)
	}
	return offsets
}

// clampToDay keeps totalMinutes within 00:00–23:59 and returns hour/minute.
func clampToDay(totalMinutes int) (int, int) {
	if totalMinutes < 0 {
		totalMinutes = 0
	}
	if max := 23*60 + 59; totalMinutes > max {
		totalMinutes = max
	}
	return totalMinutes / 60, totalMinutes % 60
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

// NotifyScheduleChanged envía por WhatsApp el horario de hoy (equivalente al
// recordatorio matinal) tras una modificación de la configuración del usuario
// desde la web. Usa los mismos offsets de jitter ya sorteados por el loop, así
// que muestra exactamente las horas que se ejecutarán. No-op si no hay cliente
// de WhatsApp, número vacío o el día no tiene fichajes programados.
func (s *Scheduler) NotifyScheduleChanged(uid string) {
	if s.wsClient == nil {
		log.Printf("Scheduler [%s]: aviso de horario omitido: cliente WhatsApp no configurado (EVOLUTION_*)", uid)
		return
	}
	uw, err := db.LoadUserWithConfig(context.Background(), s.pool, uid)
	if err != nil {
		log.Printf("Scheduler [%s]: NotifyScheduleChanged: cargando usuario: %v", uid, err)
		return
	}
	if uw.Config.WhatsappNumber == "" {
		log.Printf("Scheduler [%s]: aviso de horario omitido: usuario sin número de WhatsApp en su config", uid)
		return
	}

	now := time.Now()
	s.schedMu.Lock()
	schedule := buildSchedule(uw, now, s.jitterOffsets, s.jitterBags)
	s.schedMu.Unlock()
	if len(schedule) == 0 {
		return
	}

	locLabel := locationLabelForDay(uw.Config.OfficeDays, now.Weekday())
	text := fmt.Sprintf("✏️ Horario actualizado\n\n📍 Ubicación: %s\n%s", locLabel, formatSchedule(schedule))
	if _, wsErr := s.wsClient.SendMessage(uw.Config.WhatsappNumber, text); wsErr != nil {
		log.Printf("Scheduler [%s]: error enviando aviso de horario actualizado: %v", uid, wsErr)
	}
}

// sendWhatsappNotification sends a WhatsApp message to the user after a check-in attempt.
// It is a no-op when the client is nil, the number is empty, or EVOLUTION_ENABLED != "true".
func (s *Scheduler) sendWhatsappNotification(uid, action, status, locLabel, errMsg, whatsappNumber string) {
	if s.wsClient == nil || whatsappNumber == "" {
		log.Printf("Scheduler [%s]: notificación de resultado omitida: sin cliente WhatsApp o número vacío", uid)
		return
	}

	actionLabel := "Entrada"
	if action == "OUT" {
		actionLabel = "Salida"
	}

	var text string
	switch status {
	case "ok":
		text = fmt.Sprintf("✅ Fichaje de *%s* registrado\n📍 Ubicación: *%s*", actionLabel, locLabel)
	case "error":
		text = fmt.Sprintf("❌ Error en fichaje de *%s*\n⚠️ %s", actionLabel, errMsg)
	case "skipped":
		text = fmt.Sprintf("⏭️ Fichaje de *%s* omitido\n📅 %s", actionLabel, errMsg)
	default:
		return
	}

	if _, wsErr := s.wsClient.SendMessage(whatsappNumber, text); wsErr != nil {
		log.Printf("Scheduler [%s]: error enviando WhatsApp: %v", uid, wsErr)
	}
}

// sendMorningReminder sends a good-morning WhatsApp message ahead of the first
// check-in of the day, listing the day's location and every scheduled time
// (with jitter already applied). No-op when the client or number is missing.
func (s *Scheduler) sendMorningReminder(uid, whatsappNumber, locLabel string, entries []scheduledEntry) {
	if s.wsClient == nil || whatsappNumber == "" {
		log.Printf("Scheduler [%s]: recordatorio matinal omitido: sin cliente WhatsApp o número vacío", uid)
		return
	}

	text := fmt.Sprintf("☀️ Buenos días\n\n📍 Ubicación: %s\n%s", locLabel, formatSchedule(entries))
	if _, wsErr := s.wsClient.SendMessage(whatsappNumber, text); wsErr != nil {
		log.Printf("Scheduler [%s]: error enviando recordatorio matinal: %v", uid, wsErr)
	}
}

// firstEntryOfType returns the earliest entry matching action ("IN" or "OUT").
func firstEntryOfType(entries []scheduledEntry, action string) (scheduledEntry, bool) {
	var first scheduledEntry
	found := false
	for _, e := range entries {
		if e.action != action {
			continue
		}
		if !found || e.hour*60+e.minute < first.hour*60+first.minute {
			first = e
			found = true
		}
	}
	return first, found
}

// locationLabelForDay reports whether the given weekday is configured as an
// office day, returning "Oficina" or "Casa".
func locationLabelForDay(officeDays string, day time.Weekday) string {
	raw := strings.ReplaceAll(officeDays, "=", ",")
	for _, part := range splitCSV(raw) {
		if strings.EqualFold(strings.TrimSpace(part), day.String()) {
			return "Oficina"
		}
	}
	return "Casa"
}

// formatSchedule renders the day's check-ins sorted by time as WhatsApp lines.
func formatSchedule(entries []scheduledEntry) string {
	sorted := make([]scheduledEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].hour*60+sorted[i].minute < sorted[j].hour*60+sorted[j].minute
	})

	var b strings.Builder
	for _, e := range sorted {
		icon, label := "🟢", "Entrada"
		if e.action == "OUT" {
			icon, label = "🔴", "Salida"
		}
		fmt.Fprintf(&b, "%s %s: %02d:%02d\n", icon, label, e.hour, e.minute)
	}
	return strings.TrimRight(b.String(), "\n")
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
