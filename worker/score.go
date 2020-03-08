package worker

import (
	"database/sql"
	"log"
	"math"
	"time"

	"git.nkagami.me/natsukagami/kjudge/models"
	"github.com/jmoiron/sqlx"
)

const (
	VerdictCompileError = "Compile Error"
	VerdictScored       = "Scored"
	VerdictAccepted     = "Accepted"
)

// ScoreContext is a context for calculating a submission's score
// and update the user's problem scores.
type ScoreContext struct {
	DB      *sqlx.Tx
	Sub     *models.Submission
	Problem *models.Problem
	Contest *models.Contest
}

// Score does scoring on a submission and updates the user's ProblemResult.
func Score(s *ScoreContext) error {
	// Check if there's any test results missing.
	testResults, err := s.TestResults()
	if err != nil {
		return err
	}
	tests, err := models.GetProblemTests(s.DB, s.Problem.ID)
	if err != nil {
		return err
	}
	if missing := MissingTests(tests, testResults); len(missing) > 0 {
		log.Printf("[WORKER] Submission %v needs to run %d tests before being scored.\n", s.Sub.ID, len(missing))
		var jobs []*models.Job
		for _, m := range missing {
			jobs = append(jobs, models.NewJobRun(s.Sub.ID, m.ID))
		}
		jobs = append(jobs, models.NewJobScore(s.Sub.ID))
		return models.BatchInsertJobs(s.DB, jobs...)
	}

	log.Printf("[WORKER] Scoring submission %d\n", s.Sub.ID)
	// Calculate the score by summing scores on each test group.
	s.Sub.Score = sql.NullFloat64{Float64: 0.0, Valid: true}
	for _, tg := range tests {
		score, counts := ScoreGroup(tg, testResults)
		if counts {
			s.Sub.Score.Float64 += score
		}
	}
	// Calculate penalty too
	if err := s.ComputePenalties(s.Sub); err != nil {
		return err
	}
	// Verdict
	UpdateVerdict(tests, s.Sub)
	// Write the submission's score
	if err := s.Sub.Write(s.DB); err != nil {
		return err
	}
	log.Printf("[WORKER] Submission %d scored (verdict = %s, score = %.1f). Updating problem results\n", s.Sub.ID, s.Sub.Verdict, s.Sub.Score.Float64)

	// Update the ProblemResult
	subs, err := models.GetUserProblemSubmissions(s.DB, s.Sub.UserID, s.Problem.ID)
	if err != nil {
		return err
	}
	pr := s.CompareScores(subs)
	log.Printf("[WORKER] Problem results updated for user %s, problem %d (score = %.1f, penalty = %d)\n", s.Sub.UserID, s.Problem.ID, pr.Score, pr.Penalty)

	return pr.Write(s.DB)
}

// Update the submission's verdict.
func UpdateVerdict(tests []*models.TestGroupWithTests, sub *models.Submission) {
	score, _, counts := scoreOf(sub)
	if !counts {
		sub.Verdict = VerdictCompileError
		return
	}

	maxPossibleScore := 0.0
	for _, tg := range tests {
		if tg.Score > 0 {
			maxPossibleScore += tg.Score
		}
	}

	if score == maxPossibleScore {
		sub.Verdict = VerdictAccepted
	} else {
		sub.Verdict = VerdictScored
	}
}

// TestResults returns the submission's test results, mapped by the test's ID.
func (s *ScoreContext) TestResults() (map[int]*models.TestResult, error) {
	trs, err := models.GetSubmissionTestResults(s.DB, s.Sub.ID)
	if err != nil {
		return nil, err
	}
	res := make(map[int]*models.TestResult)
	for _, tr := range trs {
		res[tr.TestID] = tr
	}
	return res, nil
}

// ComputePenalties compute penalty values for each submission, based on the PenaltyPolicy.
func (s *ScoreContext) ComputePenalties(sub *models.Submission) error {
	value := 0
	switch s.Problem.PenaltyPolicy {
	case models.PenaltyPolicyNone:
	case models.PenaltyPolicyICPC:
		subs, err := models.GetUserProblemSubmissions(s.DB, s.Sub.UserID, s.Problem.ID)
		if err != nil {
			return err
		}
		for id, s := range subs {
			if sub.ID == s.ID {
				value = 20 * id
				break
			}
		}
		fallthrough // We also need the submit time
	case models.PenaltyPolicySubmitTime:
		value += int((sub.SubmittedAt.Sub(s.Contest.StartTime) + time.Minute - 1) / time.Minute)
	default:
		panic(s)
	}
	sub.Penalty = sql.NullInt64{Int64: int64(value), Valid: true}
	return nil
}

// Returns (score, penalty, should_count).
func scoreOf(sub *models.Submission) (float64, int, bool) {
	if sub == nil || sub.CompiledSource == nil || !sub.Score.Valid || !sub.Penalty.Valid {
		// Looks like a pending submission
		return 0, 0, false
	}
	return sub.Score.Float64, int(sub.Penalty.Int64), true
}

// CompareScores compare the submission results and return the best one.
// If nil is returned, then the problem result should just be removed.
func (s *ScoreContext) CompareScores(subs []*models.Submission) *models.ProblemResult {
	maxScore := 0.0
	var which *models.Submission
	contestTime := float64(s.Contest.EndTime.Sub(s.Contest.StartTime))
	counted := 0
	for _, sub := range subs {
		score, _, counts := scoreOf(sub)
		if !counts {
			continue
		}
		switch s.Problem.ScoringMode {
		case models.ScoringModeOnce:
			if which == nil {
				which = sub
				maxScore = score
				break
			}
		case models.ScoringModeLast:
			which = sub
			maxScore = score
		case models.ScoringModeDecay:
			score = math.Min(0.3,
				(1.0-0.7*float64(sub.SubmittedAt.Sub(s.Contest.StartTime))/contestTime)*
					(1.0-0.1*float64(counted)))
			fallthrough
		case models.ScoringModeBest:
			if which == nil || score >= which.Score.Float64 {
				which = sub
				maxScore = score
			}
		default:
			panic(s)
		}
		counted++
	}

	_, penalty, counts := scoreOf(which)
	if !counts {
		return &models.ProblemResult{
			BestSubmissionID: sql.NullInt64{},
			Penalty:          0,
			Score:            0.0,
			Solved:           false,
			ProblemID:        s.Problem.ID,
			UserID:           s.Sub.UserID,
		}
	}

	return &models.ProblemResult{
		BestSubmissionID: sql.NullInt64{Int64: int64(which.ID), Valid: true},
		Penalty:          penalty,
		Score:            maxScore,
		Solved:           which.Verdict == VerdictAccepted,
		ProblemID:        s.Problem.ID,
		UserID:           s.Sub.UserID,
	}
}

// ScoreGroup returns the score for a group.
// If it returns false, the group's result should be hidden.
func ScoreGroup(tg *models.TestGroupWithTests, results map[int]*models.TestResult) (float64, bool) {
	if tg.Score < 0 {
		return 0, false
	}
	switch tg.ScoringMode {
	case models.TestScoringModeSum:
		score := 0.0
		for _, test := range tg.Tests {
			result := results[test.ID]
			score += result.Score
		}
		return tg.Score * (score / float64(len(tg.Tests))), true
	case models.TestScoringModeMin:
		ratio := 1.0
		for _, test := range tg.Tests {
			result := results[test.ID]
			if ratio < result.Score {
				ratio = result.Score
			}
		}
		return tg.Score * ratio, true
	case models.TestScoringModeProduct:
		ratio := 1.0
		for _, test := range tg.Tests {
			result := results[test.ID]
			ratio *= result.Score
		}
		return tg.Score * ratio, true
	}
	panic("Unknown Scoring Mode: " + tg.ScoringMode)
}

// MissingTests finds all the tests that are missing a TestResult.
func MissingTests(tests []*models.TestGroupWithTests, results map[int]*models.TestResult) []*models.Test {
	var res []*models.Test
	for _, tg := range tests {
		for _, test := range tg.Tests {
			if _, ok := results[test.ID]; !ok {
				res = append(res, test)
			}
		}
	}
	return res
}