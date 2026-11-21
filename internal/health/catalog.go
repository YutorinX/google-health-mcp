package health

import (
	"fmt"
	"sort"
	"strings"
)

// FilterKind says which time field a data type is filtered on in
// dataPoints.list / dataPoints.reconcile.
type FilterKind int

const (
	// KindInterval: data points that cover a span of time (steps, exercise...).
	KindInterval FilterKind = iota
	// KindSample: instantaneous measurements (heart rate, weight...).
	KindSample
	// KindDaily: one summary per calendar day (daily-resting-heart-rate...).
	KindDaily
	// KindSleep: sleep sessions, filtered on their end time.
	KindSleep
	// KindECG: electrocardiogram readings; only a lower bound is supported.
	KindECG
	// KindNone: not time based (food catalog entries).
	KindNone
)

func (k FilterKind) String() string {
	switch k {
	case KindInterval:
		return "interval"
	case KindSample:
		return "sample"
	case KindDaily:
		return "daily"
	case KindSleep:
		return "sleep"
	case KindECG:
		return "ecg"
	default:
		return "none"
	}
}

// DataType describes one Google Health API data type.
type DataType struct {
	ID          string     `json:"id"`
	Scope       string     `json:"scope"`
	Kind        FilterKind `json:"-"`
	List        bool       `json:"list"`
	RollUp      bool       `json:"rollUp"`
	DailyRollUp bool       `json:"dailyRollUp"`
	Reconcile   bool       `json:"reconcile"`
	Description string     `json:"description"`
}

// FieldName is the proto field name used in filter expressions,
// e.g. "heart-rate" -> "heart_rate".
func (d DataType) FieldName() string { return strings.ReplaceAll(d.ID, "-", "_") }

// MaxRollupDays is the longest range the API accepts for rollUp and
// dailyRollUp on this data type.
func (d DataType) MaxRollupDays() int {
	switch d.ID {
	case "calories-in-heart-rate-zone", "heart-rate", "active-minutes", "total-calories":
		return 14
	}
	return 90
}

// MaxPageSize is the largest pageSize list/reconcile accept for this type.
func (d DataType) MaxPageSize() int {
	switch d.ID {
	case "sleep", "exercise":
		return 25
	}
	return 10000
}

// Scope groups (suffixes of https://www.googleapis.com/auth/googlehealth.).
const (
	scopeActivity  = "activity_and_fitness"
	scopeMetrics   = "health_metrics_and_measurements"
	scopeSleep     = "sleep"
	scopeNutrition = "nutrition"
	scopeECG       = "ecg"
	scopeIRN       = "irn"
)

// ops string: L=list R=rollUp D=dailyRollUp C=reconcile
func dt(id, scope string, kind FilterKind, ops, desc string) DataType {
	return DataType{
		ID: id, Scope: scope, Kind: kind, Description: desc,
		List:        strings.Contains(ops, "L"),
		RollUp:      strings.Contains(ops, "R"),
		DailyRollUp: strings.Contains(ops, "D"),
		Reconcile:   strings.Contains(ops, "C"),
	}
}

// Catalog lists the readable data types, from
// https://developers.google.com/health/data-types. The filter kind is our
// best mapping; ListDataPoints falls back to the other kinds if the API
// rejects the first guess.
var Catalog = map[string]DataType{}

func init() {
	for _, d := range []DataType{
		dt("steps", scopeActivity, KindInterval, "LRDC", "Step counts"),
		dt("distance", scopeActivity, KindInterval, "LRDC", "Distance travelled"),
		dt("floors", scopeActivity, KindInterval, "RDC", "Floors climbed (aggregates only)"),
		dt("altitude", scopeActivity, KindInterval, "LRDC", "Altitude gained"),
		dt("active-minutes", scopeActivity, KindInterval, "LRDC", "Active minutes by intensity"),
		dt("active-zone-minutes", scopeActivity, KindInterval, "LRDC", "Active Zone Minutes"),
		dt("activity-level", scopeActivity, KindInterval, "LC", "Sedentary/light/moderate/vigorous periods"),
		dt("active-energy-burned", scopeActivity, KindInterval, "LRDC", "Calories burned through activity"),
		dt("total-calories", scopeActivity, KindInterval, "RD", "Total calories burned, basal + active (aggregates only)"),
		dt("calories-in-heart-rate-zone", scopeActivity, KindInterval, "RD", "Calories burned per heart-rate zone (aggregates only)"),
		dt("time-in-heart-rate-zone", scopeActivity, KindInterval, "LRDC", "Time spent in each heart-rate zone"),
		dt("sedentary-period", scopeActivity, KindInterval, "LRDC", "Sedentary periods"),
		dt("exercise", scopeActivity, KindInterval, "LC", "Workouts / exercise sessions"),
		dt("swim-lengths-data", scopeActivity, KindInterval, "LRDC", "Swim lengths"),
		dt("vo2-max", scopeActivity, KindSample, "LC", "VO2 max estimates"),
		dt("run-vo2-max", scopeActivity, KindSample, "LRDC", "VO2 max estimated from runs"),
		dt("daily-vo2-max", scopeActivity, KindDaily, "LC", "Daily cardio fitness (VO2 max) summary"),

		dt("heart-rate", scopeMetrics, KindSample, "LRDC", "Heart rate samples (very dense; prefer rollups)"),
		dt("heart-rate-variability", scopeMetrics, KindSample, "LC", "Intraday heart rate variability"),
		dt("daily-resting-heart-rate", scopeMetrics, KindDaily, "LC", "Resting heart rate per day"),
		dt("daily-heart-rate-variability", scopeMetrics, KindDaily, "LC", "Nightly HRV summary per day"),
		dt("daily-heart-rate-zones", scopeMetrics, KindDaily, "LC", "Personal heart-rate zone thresholds per day"),
		dt("oxygen-saturation", scopeMetrics, KindSample, "LC", "SpO2 samples"),
		dt("daily-oxygen-saturation", scopeMetrics, KindDaily, "LC", "SpO2 summary per day"),
		dt("daily-respiratory-rate", scopeMetrics, KindDaily, "LC", "Breathing rate per day"),
		dt("respiratory-rate-sleep-summary", scopeMetrics, KindSample, "LC", "Breathing rate during sleep"),
		dt("daily-sleep-temperature-derivations", scopeMetrics, KindDaily, "LC", "Nightly skin temperature variation"),
		dt("core-body-temperature", scopeMetrics, KindSample, "LRDC", "Core body temperature"),
		dt("weight", scopeMetrics, KindSample, "LRDC", "Body weight"),
		dt("body-fat", scopeMetrics, KindSample, "LRDC", "Body fat percentage"),
		dt("height", scopeMetrics, KindSample, "LC", "Height"),
		dt("blood-glucose", scopeMetrics, KindSample, "LRDC", "Blood glucose"),

		dt("sleep", scopeSleep, KindSleep, "LC", "Sleep sessions with stages"),

		dt("nutrition-log", scopeNutrition, KindInterval, "LRDC", "Logged food and nutrients"),
		dt("hydration-log", scopeNutrition, KindInterval, "LRDC", "Logged water intake"),
		dt("food", scopeNutrition, KindNone, "L", "Foods in the user's food catalog"),
		dt("food-measurement-unit", scopeNutrition, KindNone, "L", "Food measurement units"),

		dt("electrocardiogram", scopeECG, KindECG, "L", "ECG readings"),
		dt("irregular-rhythm-notification", scopeIRN, KindInterval, "L", "Irregular rhythm notifications"),
	} {
		Catalog[d.ID] = d
	}
}

// Lookup returns the data type or an error listing valid ids.
func Lookup(id string) (DataType, error) {
	id = strings.TrimSpace(strings.ToLower(id))
	if d, ok := Catalog[id]; ok {
		return d, nil
	}
	ids := make([]string, 0, len(Catalog))
	for k := range Catalog {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	return DataType{}, fmt.Errorf("unknown data type %q; valid types: %s", id, strings.Join(ids, ", "))
}

// SortedCatalog returns the catalog ordered by scope then id.
func SortedCatalog() []DataType {
	out := make([]DataType, 0, len(Catalog))
	for _, d := range Catalog {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// filterFor builds a list/reconcile filter for [start, end) civil dates
// (YYYY-MM-DD) using the given kind.
func filterFor(d DataType, kind FilterKind, start, end string) string {
	f := d.FieldName()
	rng := func(field string) string {
		return fmt.Sprintf(`%s >= "%s" AND %s < "%s"`, field, start, field, end)
	}
	switch kind {
	case KindInterval:
		return rng(f + ".interval.civil_start_time")
	case KindSample:
		return rng(f + ".sample_time.civil_time")
	case KindDaily:
		return rng(f + ".date")
	case KindSleep:
		return rng("sleep.interval.civil_end_time")
	case KindECG:
		// Only a lower bound on physical start time is supported.
		return fmt.Sprintf(`electrocardiogram.interval.start_time >= "%sT00:00:00Z"`, start)
	}
	return ""
}

// filterCandidates returns the filter to try first, then alternatives in
// case the catalog's guess of the time field is wrong.
func filterCandidates(d DataType, start, end string) []string {
	out := []string{filterFor(d, d.Kind, start, end)}
	switch d.Kind {
	case KindInterval, KindSample, KindDaily:
		for _, k := range []FilterKind{KindInterval, KindSample, KindDaily} {
			if k != d.Kind {
				out = append(out, filterFor(d, k, start, end))
			}
		}
	}
	return out
}
