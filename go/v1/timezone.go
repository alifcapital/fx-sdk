package v1

import "time"

// The FX Core operates in UTC+05:00 (Tajikistan time, which observes no DST).
// Every date, hour and window bound the Core sends, expects or compares is in
// that offset, so the SDK pins itself to the same one instead of taking it from
// the host machine or the PostgreSQL session. The Core is the authority here —
// this offset is not a local preference and must not be made configurable.
//
// This is not cosmetic. Reconciliation compares a checksum over a one-hour
// window of local trades against the Core's checksum over the same window, and
// the two sides only agree if they mean the same hour. Left to the session
// timezone, a partner whose database runs in UTC would ask for a window five
// hours off the one the Core computes and every hour would "diverge"; the same
// drift moves client_orders.order_day onto the wrong calendar day around
// midnight, which then breaks the order lookup a trade is joined through.
//
// So the offset is fixed in two places that must agree:
//
//   - Go side: TimeZone, used for every wall-clock decision the SDK makes.
//   - SQL side: atTZ, applied to every TIMESTAMPTZ the SDK compares against a
//     date or renders as text.
//
// A partner's own queries against these tables should use the same anchoring;
// nothing here depends on the session's TimeZone setting, so it does not matter
// what the pool is configured with.
const (
	// tzOffsetHours is the SDK's fixed offset east of UTC.
	tzOffsetHours = 5
	// tzName labels the offset in formatted timestamps.
	tzName = "+05"
	// atTZ anchors a timestamp expression to the SDK timezone in SQL. It is
	// concatenated into query literals at compile time, never built from input.
	//
	// The INTERVAL form is deliberate and must not be "simplified" to the
	// string form. PostgreSQL reads AT TIME ZONE '+05' with the POSIX sign
	// convention, which is inverted: for a 10:00 wall clock it yields 15:00Z,
	// where INTERVAL '+05:00' correctly yields 05:00Z. Getting this backwards
	// shifts every reconciliation window by ten hours, silently.
	atTZ = `AT TIME ZONE INTERVAL '+05:00'`
)

// TimeZone is the fixed UTC+05:00 zone every day and hour in the SDK is
// expressed in. Use it whenever you derive a Day or Hour from the clock, so the
// value matches what the SDK and the Core will compare:
//
//	day := time.Now().In(v1.TimeZone).Format("2006-01-02")
//
// Today and PreviousHour do this for the two common cases.
var TimeZone = time.FixedZone(tzName, tzOffsetHours*3600)

// Today returns the current trading day in the SDK timezone, as YYYY-MM-DD.
// Around midnight it deliberately disagrees with a UTC or host-local date —
// that is the point.
func Today() string {
	return today(time.Now())
}

func today(now time.Time) string {
	return now.In(TimeZone).Format(dayLayout)
}

// PreviousHour returns the day and hour of the last *completed* hour in the SDK
// timezone. It is the slot to hand Reconcile and ReconcileRepair, which must
// run against a closed hour rather than the one still in progress:
//
//	day, hour := v1.PreviousHour()
//	res, err := c.ReconcileRepair(ctx, &v1.ReconcileParams{
//		PartnerId: partnerId, Day: day, Hour: hour,
//	}, handleTrade)
//
// It also rolls the day back correctly: at 00:30 on the 5th it returns hour 23
// of the 4th, not hour 23 of the 5th.
func PreviousHour() (day string, hour int) {
	return previousHour(time.Now())
}

func previousHour(now time.Time) (day string, hour int) {
	t := now.In(TimeZone).Add(-reconcileWindow)
	return t.Format(dayLayout), t.Hour()
}
