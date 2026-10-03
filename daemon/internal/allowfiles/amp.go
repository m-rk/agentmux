package allowfiles

// AmpAllowArgs returns the extra `amp` arguments needed to let a runner read
// and edit the given files outside its workdir. It is always empty.
//
// Verified against amp 0.0.1789646488 with `amp -x` from a throwaway workdir:
// amp has no outside-workdir gate. The docs say "Amp does not ask for approval
// before running tools", and a run read and edited a sibling file outside the
// workdir without any permission configuration. amp.permissions rules (settings
// file, selectable with --settings-file or AMP_SETTINGS_FILE) are allow, reject,
// ask or delegate rules per tool; allow rules are the default, so granting one
// file is unnecessary, and confining amp to the workdir would need reject rules
// that this package does not try to build.
//
// The launcher should therefore add nothing for amp and, if it surfaces this to
// the operator, say that amp instances can touch files outside the workdir
// regardless of -allow-file. paths is accepted so the caller has one shape for
// every agent; it is not inspected.
func AmpAllowArgs(paths []string) []string {
	return nil
}
