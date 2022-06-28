package command

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/api/contexts"
	flaghelper "github.com/hashicorp/nomad/helper/flags"
	"github.com/posener/complete"
)

type EvalDeleteCommand struct {
	Meta

	scheduler  flaghelper.StringFlag
	status     flaghelper.StringFlag
	job        flaghelper.StringFlag
	deployment flaghelper.StringFlag
	node       flaghelper.StringFlag
	filterOp   string
	yes        bool

	// originalBrokerPaused indicates whether the broker was in a paused state
	// before the command was run. This indicates what action, if any, should
	// be taken before the command finishes.
	originalBrokerPaused bool

	// deleteByArg is set when the command is deleting an evaluation that has
	// been passed as an argument. This avoids need for confirmation.
	deleteByArg bool

	// numDeleted tracks the total evaluations deleted in a single run of this
	// command. It provides a way to output this information to the user at the
	// command completion.
	numDeleted int

	// client is the lazy-loaded API client and is stored here, so we don't
	// need to pass it to multiple functions.
	client *api.Client
}

func (e *EvalDeleteCommand) Help() string {
	helpText := `
Usage: nomad eval delete [options] <evaluation>

  Delete an evaluation by ID. If the evaluation ID is omitted, this command
  will use the filter options below to delete a set of evaluations. If ACLs
  are enabled, this command requires a management ACL token.

  This command should be used cautiously and only in outage situations where
  there is a large backlog of evaluations not being processed. During most
  normal and outage scenarios, Nomads reconciliation and state management will
  handle evaluations as needed.

General Options:

  ` + generalOptionsUsage(usageOptsNoNamespace) + `

Eval Delete Options:

  -scheduler
    Filter by scheduler type (one of "batch", "service", "system", or
    "sysbatch"). This option may be specified multiple times.

  -status
    Filter by eval status (one of "blocked", "pending", "complete",
    "failed", or "canceled"). This option may be specified multiple times.

  -job
    Filter by an associated job ID. This option may be specified
    multiple times.

  -deployment
    Filter by an associated deployment ID. This option may be specified
    multiple times.

  -node
    Filter by an associated node ID. This option may be specified
    multiple times.

  -filter-operator
    The filter operator to use when multiple filter options are specified. This
    defaults to "and" but also supports "or".

  -yes
    Bypass the confirmation prompt if an evaluation ID was not provided.
`

	return strings.TrimSpace(helpText)
}

func (e *EvalDeleteCommand) Synopsis() string {
	return "Delete evaluations by ID or using filters"
}

func (e *EvalDeleteCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(e.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-scheduler": complete.PredictSet(
				api.JobTypeService,
				api.JobTypeBatch,
				api.JobTypeSystem,
				api.JobTypeSysBatch,
			),
			"-status":          complete.PredictAnything,
			"-job":             complete.PredictAnything,
			"-deployment":      complete.PredictAnything,
			"-node":            complete.PredictAnything,
			"-filter-operator": complete.PredictSet("and", "or"),
			"-yes":             complete.PredictNothing,
		})
}

func (e *EvalDeleteCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictFunc(func(a complete.Args) []string {
		client, err := e.Meta.Client()
		if err != nil {
			return nil
		}

		resp, _, err := client.Search().PrefixSearch(a.Last, contexts.Evals, nil)
		if err != nil {
			return []string{}
		}
		return resp.Matches[contexts.Evals]
	})
}

func (e *EvalDeleteCommand) Name() string { return "eval delete" }

func (e *EvalDeleteCommand) Run(args []string) int {

	flags := e.Meta.FlagSet(e.Name(), FlagSetClient)
	flags.Usage = func() { e.Ui.Output(e.Help()) }
	flags.Var(&e.scheduler, "scheduler", "")
	flags.Var(&e.status, "status", "")
	flags.Var(&e.job, "job", "")
	flags.Var(&e.deployment, "deployment", "")
	flags.Var(&e.node, "node", "")
	flags.StringVar(&e.filterOp, "filter-operator", "and", "")
	flags.BoolVar(&e.yes, "yes", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	args = flags.Args()

	if err := e.verifyArgsAndFlags(args); err != nil {
		e.Ui.Error(fmt.Sprintf("Error validating command args and flags: %v", err))
		return 1
	}

	// Pad the filter operator with whitespace, so it can be used as is when
	// building the filter. This transforms "and" to " and ".
	originalOP := e.filterOp
	e.filterOp = " " + originalOP + " "

	// Get the HTTP client and store this for use across multiple functions.
	client, err := e.Meta.Client()
	if err != nil {
		e.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	e.client = client

	// Pause the eval broker to ensure no more work can be dequeued and
	// processed by the schedulers.
	if err := e.pauseEvalBroker(); err != nil {
		e.Ui.Error(fmt.Sprintf("Error pausing eval broker: %s", err))
		return 1
	}

	// Track the eventual exit code as there are a number of factors that
	// influence this.
	var exitCode int

	// Call the correct function in order to handle the operator input
	// correctly.
	switch len(args) {
	case 1:
		e.deleteByArg = true
		exitCode, err = e.handleEvalArgDelete(args[0])
	default:
		exitCode, err = e.handleFlagFilterDelete("", e.buildFilter())
	}

	// Do not exit if we got an error as it's possible this was on the
	// non-first iteration, and we have therefore deleted some evals.
	if err != nil {
		e.Ui.Error(fmt.Sprintf("Error deleting evaluations: %s", err))
	}

	// Depending on whether we deleted evaluations or not, output a message so
	// this is clear.
	if e.numDeleted > 0 {
		grammar := "evaluation"
		if e.numDeleted > 1 {
			grammar = grammar + "s"
		}
		e.Ui.Output(fmt.Sprintf("Successfully deleted %v %s", e.numDeleted, grammar))
	} else if err == nil {
		e.Ui.Output("No evaluations were deleted")
	}

	// Always attempt to revert the state of the eval broker. In the event this
	// succeeds, but the eval deletion failed, maintain the non-zero exit code
	// so failures are clear.
	if err := e.revertEvalBroker(); err != nil {
		e.Ui.Error(fmt.Sprintf("Error reverting eval broker state: %s", err))
		exitCode = 1
	}
	return exitCode
}

// verifyArgsAndFlags ensures the passed arguments and flags are valid for what
// this command accepts and can take action on.
func (e *EvalDeleteCommand) verifyArgsAndFlags(args []string) error {

	numArgs := len(args)
	numFlags := len(e.scheduler) + len(e.status) + len(e.job) + len(e.deployment) + len(e.node)

	// The command takes either an argument or flags, but not both.
	if (numFlags < 1 && numArgs < 1) || (numFlags > 0 && numArgs > 0) {
		return errors.New("evaluation ID or filter flags required")
	}

	// If an argument is supplied, we only accept a single eval ID.
	if numArgs > 1 {
		return fmt.Errorf("expected 1 argument, got %v", numArgs)
	}

	// Ensure the filter operator matches supported and expected values.
	switch e.filterOp {
	case "and", "or":
	default:
		return fmt.Errorf(`got filter-operator %q, supports "and" "or"`, e.filterOp)
	}

	return nil
}

// pauseEvalBroker pauses the eval broker if it is currently in an enabled
// state. Its previous state will be stored so the command can later revert any
// changes made.
func (e *EvalDeleteCommand) pauseEvalBroker() error {

	schedulerConfig, _, err := e.client.Operator().SchedulerGetConfiguration(nil)
	if err != nil {
		return err
	}

	// Store the current and thus original eval broker paused state.
	e.originalBrokerPaused = schedulerConfig.SchedulerConfig.PauseEvalBroker

	// It is possible the operator has already paused the broker via the operator
	// API and CLI. If this is not the case, pause it now.
	if !e.originalBrokerPaused {
		schedulerConfig.SchedulerConfig.PauseEvalBroker = true

		newSchedulerConfig, _, err := e.client.Operator().SchedulerCASConfiguration(schedulerConfig.SchedulerConfig, nil)
		if err != nil {
			return err
		}

		if !newSchedulerConfig.Updated {
			return errors.New("failed to update scheduler config, please check server logs")
		}
	}

	return nil
}

// revertEvalBroker reverts any initial changes made to the state of the eval
// broker.
func (e *EvalDeleteCommand) revertEvalBroker() error {

	// Read the configuration state first to ensure we are using the correct
	// configuration and not overwriting changes.
	schedulerConfig, _, err := e.client.Operator().SchedulerGetConfiguration(nil)
	if err != nil {
		return err
	}

	// If the broker is paused, and it was paused before this command was
	// invoked, there is nothing else to do.
	if schedulerConfig.SchedulerConfig.PauseEvalBroker && e.originalBrokerPaused {
		return nil
	}

	// Modify the paused value and write this.
	schedulerConfig.SchedulerConfig.PauseEvalBroker = false

	newSchedulerConfig, _, err := e.client.Operator().SchedulerCASConfiguration(schedulerConfig.SchedulerConfig, nil)
	if err != nil {
		return err
	}

	if !newSchedulerConfig.Updated {
		return errors.New("failed to update scheduler config, please check server logs")
	}
	return nil
}

// handleEvalArgDelete handles deletion and evaluation which was passed via
// it's ID as a command argument. This is the simplest route to take and
// doesn't require filtering or batching.
func (e *EvalDeleteCommand) handleEvalArgDelete(evalID string) (int, error) {
	evalInfo, _, err := e.client.Evaluations().Info(evalID, nil)
	if err != nil {
		return 1, err
	}
	return e.batchDelete([]*api.Evaluation{evalInfo})
}

// handleFlagFilterDelete handles deletion of evaluations discovered using
// filter flags. It is unknown how many will match the operator criteria so
// this function batches lookup and delete requests into sensible numbers.
//
// The function is recursive and will run until it has found and deleted all
// evals which matched the criteria. It is possible for a batch of evals to be
// deleted, but a subsequent call may fail due to temporary issues. The command
// is idempotent and may be re-run safely if this occurs.
func (e *EvalDeleteCommand) handleFlagFilterDelete(nextToken, filter string) (int, error) {

	// Generate the query options using the passed next token and filter. The
	// per page value is less than the total number we can include in a single
	// delete request. This keeps the maximum size of the return object at a
	// reasonable size.
	//
	// The JSON serialized evaluation API object is 350-380B in size.
	// 2426 * 380B (3.8e-4 MB) = 0.92MB. We may want to make this configurable
	// in the future, but this is counteracted by the CLI logic which will loop
	// until the user tells it to exit, or all evals matching the filter are
	// deleted. 2426 * 3 falls below the maximum limit for eval IDs in a single
	// delete request (set by MaxEvalIDsPerDeleteRequest).
	opts := &api.QueryOptions{
		Filter:    filter,
		PerPage:   2426,
		NextToken: nextToken,
	}

	var evalsToDelete []*api.Evaluation

	// Call List 3 times to accumulate the maximum number if eval IDs supported
	// in a single Delete request. See math above.
	for i := 0; i < 3; i++ {

		evalList, meta, err := e.client.Evaluations().List(opts)
		if err != nil {
			return 1, err
		}
		evalsToDelete = append(evalsToDelete, evalList...)

		// Store the next token no matter if it is empty or populated.
		opts.NextToken = meta.NextToken

		// If there is no next token, ensure we exit and avoid any new loops
		// which will result in duplicate IDs.
		if opts.NextToken == "" {
			break
		}
	}

	// The filter flags are operator controlled, therefore ensure we actually
	// found some evals to delete. Otherwise, inform the operator their flags
	// are potentially incorrect.
	if len(evalsToDelete) == 0 {
		if e.numDeleted > 0 {
			return 0, nil
		} else {
			return 1, errors.New("failed to find any evals that matched filter criteria")
		}
	}

	if code, err := e.batchDelete(evalsToDelete); err != nil {
		return code, err
	}

	// If there is another page of evaluations matching the filter, call this
	// function again and delete the next batch of evals. We pause for a 500ms
	// rather than just run as fast as the code and machine possibly can. This
	// means deleting 13million evals will take roughly 13-15 mins, which seems
	// reasonable.
	if opts.NextToken != "" {
		time.Sleep(500 * time.Millisecond)
		return e.handleFlagFilterDelete(opts.NextToken, filter)
	}
	return 0, nil
}

// buildFilter is used to create an API filter string based on the operator
// input flags. This function should be called once, and reused when required
// for some form of efficiency.
func (e *EvalDeleteCommand) buildFilter() string {

	var filters []string

	if len(e.scheduler) > 0 {
		filters = append(filters, joinStringFilter(e.scheduler, "Type ==", e.filterOp))
	}
	if len(e.status) > 0 {
		filters = append(filters, joinStringFilter(e.status, "Status ==", e.filterOp))
	}
	if len(e.job) > 0 {
		filters = append(filters, joinStringFilter(e.job, "JobID ==", e.filterOp))
	}
	if len(e.deployment) > 0 {
		filters = append(filters, joinStringFilter(e.deployment, "DeploymentID ==", e.filterOp))
	}
	if len(e.node) > 0 {
		filters = append(filters, joinStringFilter(e.node, "NodeID ==", e.filterOp))
	}

	return strings.Join(filters, e.filterOp)
}

// joinStringFilter is a modified version of the strings.Join function which is
// specific to joining the input flags together to create a single go-bexpr
// filter. It does not handle empty inputs and should be performed by the
// caller.
func joinStringFilter(elems []string, selector, sep string) string {
	switch len(elems) {
	case 1:
		return fmt.Sprintf("%s %q", selector, elems[0])
	}
	n := len(sep) * (len(elems) - 1)
	for i := 0; i < len(elems); i++ {
		n += len(elems[i])
	}

	var b strings.Builder
	b.Grow(n)
	b.WriteString(fmt.Sprintf("%s %q", selector, elems[0]))
	for _, s := range elems[1:] {
		b.WriteString(sep)
		b.WriteString(fmt.Sprintf("%s %q", selector, s))
	}
	return b.String()
}

// batchDelete is responsible for deleting the passed evaluations and asking
// any confirmation questions along the way. It will ask whether the operator
// want to list the evals before deletion, and optionally ask for confirmation
// before deleting based on input criteria.
func (e *EvalDeleteCommand) batchDelete(evals []*api.Evaluation) (int, error) {

	// Ask whether the operator wants to see the list of evaluations before
	// moving forward with deletion. This will only happen if filters are used
	// and the confirmation step is not bypassed.
	if !e.yes && !e.deleteByArg {
		_, listEvals := e.askQuestion(fmt.Sprintf(
			"Do you want to list evals (%v) before deletion? [y/N]",
			len(evals)), "")

		// List the evals for deletion is the user has requested this. It can
		// be useful when the list is small and targeted, but is maybe best
		// avoided when deleting large quantities of evals.
		if listEvals {
			e.Ui.Output("")
			outputEvalList(e.Ui, evals, shortId)
			e.Ui.Output("")
		}
	}

	// Generate our list of eval IDs which is required for the API request.
	ids := make([]string, len(evals))

	for i, eval := range evals {
		ids[i] = eval.ID
	}

	// If the user did not wish to bypass the confirmation step, ask this now
	// and handle the response.
	if !e.yes && !e.deleteByArg {
		code, deleteEvals := e.askQuestion(fmt.Sprintf(
			"Are you sure you want to delete %v evals? [y/N]",
			len(evals)), "Cancelling eval deletion")
		e.Ui.Output("")

		if !deleteEvals {
			return code, nil
		}
	}

	_, err := e.client.Evaluations().Delete(ids, nil)
	if err != nil {
		return 1, err
	}

	// Calculate how many total evaluations we have deleted, so we can output
	// this at the end of the process.
	curDeleted := e.numDeleted
	e.numDeleted = curDeleted + len(ids)

	return 0, nil
}

// askQuestion allows the command to ask the operator a question requiring a
// y/n response. The optional noResp is used when the operator responds no to
// a question.
func (e *EvalDeleteCommand) askQuestion(question, noResp string) (int, bool) {

	answer, err := e.Ui.Ask(question)
	if err != nil {
		e.Ui.Error(fmt.Sprintf("Failed to parse answer: %v", err))
		return 1, false
	}

	if answer == "" || strings.ToLower(answer)[0] == 'n' {
		if noResp != "" {
			e.Ui.Output(noResp)
		}
		return 0, false
	} else if strings.ToLower(answer)[0] == 'y' && len(answer) > 1 {
		e.Ui.Output("For confirmation, an exact ‘y’ is required.")
		return 0, false
	} else if answer != "y" {
		e.Ui.Output("No confirmation detected. For confirmation, an exact 'y' is required.")
		return 1, false
	}
	return 0, true
}
