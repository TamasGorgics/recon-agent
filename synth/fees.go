package synth

// feeMinor computes the psp_a charge fee in minor units:
//
//	fee = round_half_up(gross * pct_bps / 10000) + fixed_minor
//
// It uses integer arithmetic only (FIN-06). gross must be non-negative.
func feeMinor(gross int64, pctBps int, fixedMinor int64) int64 {
	if gross <= 0 {
		return fixedMinor
	}
	scaled := gross * int64(pctBps)
	fee := (scaled + 5_000) / 10_000
	return fee + fixedMinor
}

// feeScheduleID is the stable logical ID of one fee_schedules row. The dataset
// version participates so two configs that differ only in fee rates do not
// overwrite each other's historical row with a different value.
func feeScheduleID(seed int64, dataset string, source, kind, currency, validFrom string) string {
	return logicalID(seed, "fee_schedule", dataset, source, kind, currency, validFrom)
}

// scheduleFor finds the fee schedule a psp_a charge for currency uses.
func scheduleFor(schedules []FeeSchedule, source, kind, currency string) (FeeSchedule, bool) {
	for _, schedule := range schedules {
		if schedule.Source == source && schedule.Kind == kind && schedule.Currency == currency {
			return schedule, true
		}
	}
	return FeeSchedule{}, false
}
