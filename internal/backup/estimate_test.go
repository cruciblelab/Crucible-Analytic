package backup

import "testing"

// TestTheMarginIsOneRuleAndNotTwoSpellingsOfIt.
//
// # What this is guarding against
//
// The margin used to be two lines written out twice - once in Measure
// and once in MeasureSecrets - and the copies had drifted. Both took the
// smaller of a gigabyte and a tenth of the filesystem; only one of them
// asked first whether the size was known.
//
// So on a filesystem whose total reads as zero, which diskspace.Read
// reports rather than refuses, the data backup kept no margin at all and
// the secrets backup kept a gigabyte. Neither number was chosen. They
// were one rule, spelled twice, and the spelling nobody was looking at
// is the one that lost the guard.
//
// The zero case is the whole reason this test exists, and it is the case
// that has no filesystem to measure - which is why it is here as
// arithmetic rather than in the integration tests as a mount.
func TestTheMarginIsOneRuleAndNotTwoSpellingsOfIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		total int64
		want  int64
		why   string
	}{
		{
			name: "olculemeyen", total: 0, want: FreeMargin,
			why: "a filesystem whose size is not known keeps the fixed margin. " +
				"Zero means 'not measured', and reading it as 'nothing needs " +
				"to stay free' turns the one case where we know least into " +
				"the one case with no guard at all",
		},
		{
			name: "eksi", total: -1, want: FreeMargin,
			why: "same, and negative is even less of a size than zero",
		},
		{
			name: "kucuk", total: 64 << 20, want: 6710886,
			why: "under ten gigabytes the tenth is the smaller of the two. " +
				"Written out rather than as an expression, so the test says " +
				"what the answer is instead of computing it the same way the " +
				"code does and agreeing with itself",
		},
		{
			name: "buyuk", total: 500 << 30, want: FreeMargin,
			why: "over ten gigabytes the fixed gigabyte is the smaller, and a " +
				"tenth of a large disk would refuse backups that fit easily",
		},
		{
			name: "tam esik", total: 10 << 30, want: FreeMargin,
			why: "exactly ten gigabytes: a tenth is a gigabyte, so either " +
				"branch gives the same answer and the boundary is not a step",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MarginFor(tc.total); got != tc.want {
				t.Errorf("MarginFor(%d) = %d, want %d.\n%s",
					tc.total, got, tc.want, tc.why)
			}
		})
	}
}

// TestTheMarginIsNotZeroToBeginWith.
//
// Every case above compares against FreeMargin, so a FreeMargin of zero
// would make the table pass by agreeing that nothing needs to stay free.
func TestTheMarginIsNotZeroToBeginWith(t *testing.T) {
	if FreeMargin <= 0 {
		t.Fatalf("FreeMargin is %d, so there is no margin to keep and the "+
			"table above passes by agreeing on nothing", int64(FreeMargin))
	}
}
