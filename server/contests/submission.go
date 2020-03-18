package contests

import (
	"database/sql"
	"net/http"
	"strconv"

	"git.nkagami.me/natsukagami/kjudge/db"
	"git.nkagami.me/natsukagami/kjudge/models"
	"github.com/labstack/echo/v4"
	"github.com/pkg/errors"
)

// SubmissionCtx is the context for rendering the submission page.
type SubmissionCtx struct {
	*ContestCtx

	Submission  *models.Submission
	Problem     *models.Problem
	TestGroups  []*models.TestGroupWithTests
	TestResults map[int]*models.TestResult
}

// Collect a submission ctx.
func getSubmissionCtx(db db.DBContext, c echo.Context) (*SubmissionCtx, error) {
	contest, err := getContestCtx(db, c)
	if err != nil {
		return nil, err
	}
	idStr := c.Param("submission")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return nil, echo.ErrNotFound
	}

	sub, err := models.GetSubmission(db, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, echo.ErrNotFound
	} else if err != nil {
		return nil, err
	}

	// Disallow non-owners
	if sub.UserID != contest.Me.ID {
		return nil, echo.ErrForbidden
	}

	problem, err := models.GetProblem(db, sub.ProblemID)
	if err != nil {
		return nil, err
	}
	// Disallow submissions outside of current contest
	if problem.ContestID != contest.Contest.ID {
		return nil, echo.ErrNotFound
	}

	testGroups, err := models.GetProblemTestsMeta(db, problem.ID)
	if err != nil {
		return nil, err
	}

	var testResults map[int]*models.TestResult
	if sub.Score.Valid {
		trs, err := models.GetSubmissionTestResults(db, sub.ID)
		if err != nil {
			return nil, err
		}
		testResults = make(map[int]*models.TestResult)
		for _, tr := range trs {
			testResults[tr.TestID] = tr
		}
	}

	return &SubmissionCtx{
		ContestCtx:  contest,
		Submission:  sub,
		Problem:     problem,
		TestGroups:  testGroups,
		TestResults: testResults,
	}, nil
}

// Render renders the context.
func (ctx *SubmissionCtx) Render(c echo.Context) error {
	return c.Render(http.StatusOK, "contests/submission", ctx)
}

func (g *Group) SubmissionGet(c echo.Context) error {
	ctx, err := getSubmissionCtx(g.db, c)
	if err != nil {
		return err
	}
	return ctx.Render(c)
}