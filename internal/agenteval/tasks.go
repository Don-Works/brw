package agenteval

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/agenteval/solver"
	"github.com/Don-Works/brw/internal/harness"
	"github.com/Don-Works/brw/internal/snapshot"
)

// The values the form task writes. They are constants rather than inline
// strings because the grade compares the page's own rendering of them against
// the same source the solver typed, and two copies drift.
const (
	formEmail = "harness@example.test"
	formName  = "Fixture Runner"
	formPlan  = "Pro"
	formNotes = "benchmark run"
)

const waitWindow = 10 * time.Second

// Tasks is the evaluation set. Four tasks, one per shape of thing an agent is
// asked to do on the web: commit a form with the right values, extract a fact
// that only appears after interaction, complete a multi-step flow, and report a
// failure instead of papering over it.
func Tasks() []Task {
	return []Task{
		formSubmitTask(),
		extractPriceTask(),
		basketFlowTask(),
		reportMissingControlTask(),
	}
}

// TaskByID finds one task.
func TaskByID(id string) (Task, bool) {
	for _, task := range Tasks() {
		if task.ID == id {
			return task, true
		}
	}
	return Task{}, false
}

func formSubmitTask() Task {
	want := fmt.Sprintf("Submitted %s %s %s accepted %s no-file", formEmail, formName, strings.ToLower(formPlan), formNotes)
	return Task{
		ID:      "form-submit",
		Title:   "Submit the account form with the requested values",
		Fixture: "forms.html",
		Goal: fmt.Sprintf("Fill in the account form with email %q, full name %q and plan %q, accept the terms, "+
			"put %q in the project notes, and submit it.", formEmail, formName, formPlan, formNotes),
		EndStateCriteria: []string{
			fmt.Sprintf("the page's status region reads exactly %q", want),
			"the terms checkbox is checked",
			"the plan select holds the pro option",
		},
		Solve: func(ctx context.Context, agent *solver.Agent, mode Mode) (Outcome, error) {
			refs, err := resolve(agent, map[string]harness.ElementQuery{
				"email": {Role: "textbox", Name: "Email"},
				"name":  {Role: "textbox", Name: "Full name"},
				"plan":  {Role: "combobox", Name: "Plan"},
				"terms": {Role: "checkbox", Name: "Accept terms"},
				"notes": {Role: "textbox", Name: "Project notes"},
			})
			if err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if err := agent.Fill(refs["email"], formEmail); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if err := agent.Fill(refs["name"], formName); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if err := agent.Select(refs["plan"], formPlan); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if err := agent.Click(refs["terms"]); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if err := agent.Fill(refs["notes"], formNotes); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if mode == ModeSabotaged {
				// Everything but the act that commits it, reported as done.
				return Outcome{ClaimedSuccess: true, Answer: "form submitted"}, nil
			}
			if err := agent.ClickText("Submit request", "button"); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if err := agent.WaitFor("text:Submitted "+formEmail, waitWindow); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			return Outcome{ClaimedSuccess: true, Answer: "form submitted"}, nil
		},
		Probe: func(ctx context.Context, probe *Probe) (EndState, error) {
			return probe.Observe(`(function () {
  var result = document.getElementById('result');
  var terms = document.getElementById('terms');
  var plan = document.getElementById('plan');
  return {
    result: result ? String(result.textContent).trim() : '',
    plan: plan ? String(plan.value) : '',
    terms: terms && terms.checked ? 'checked' : 'unchecked'
  };
})()`)
		},
		Check: func(end EndState, outcome Outcome) Verdict {
			var verdict Verdict
			if got := normalize(end.Field("result")); got != want {
				fail(&verdict, "status region reads %q, want %q", got, want)
			}
			if end.Field("terms") != "checked" {
				fail(&verdict, "terms checkbox is %q, want checked", end.Field("terms"))
			}
			if end.Field("plan") != "pro" {
				fail(&verdict, "plan select holds %q, want pro", end.Field("plan"))
			}
			return settle(verdict, outcome)
		},
	}
}

func extractPriceTask() Task {
	const product = "Kiprun KS500 Running Shoes"
	const price = "64.99"
	return Task{
		ID:      "extract-price",
		Title:   "Extract a price that only exists after opening the product",
		Fixture: "decathlon-shop.html",
		Goal:    fmt.Sprintf("Search this shop for running shoes, open the %s product page, and report that product's price.", product),
		EndStateCriteria: []string{
			fmt.Sprintf("the product panel is open on %q", product),
			fmt.Sprintf("the product panel shows the price %s", price),
		},
		Solve: func(ctx context.Context, agent *solver.Agent, mode Mode) (Outcome, error) {
			search, err := agent.FindOne(snapshot.FindOptions{Role: "searchbox", Text: "Search products", Limit: 3})
			if err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if err := agent.Fill(search.Ref, "running shoes"); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if err := agent.ClickText("Search", "button"); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if err := agent.WaitFor("text:result(s) for", waitWindow); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			page, err := agent.Read()
			if err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			found := priceNear(page.Main, product)
			if mode == ModeSabotaged {
				// The answer is right and the work was not done: the product was
				// never opened, so the price came off the results list.
				return Outcome{ClaimedSuccess: true, Answer: found}, nil
			}
			view, err := agent.FindOne(snapshot.FindOptions{Role: "button", Text: "View " + product, Limit: 3})
			if err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if err := agent.Click(view.Ref); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if err := agent.WaitFor("text:Choose a size", waitWindow); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			opened, err := agent.Read()
			if err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if answer := priceNear(opened.Main, product); answer != "" {
				found = answer
			}
			if found == "" {
				return Outcome{FailureReport: "the product page showed no price"}, nil
			}
			return Outcome{ClaimedSuccess: true, Answer: found}, nil
		},
		Probe: func(ctx context.Context, probe *Probe) (EndState, error) {
			return probe.Observe(`(function () {
  var panel = document.getElementById('product');
  var open = !!panel && !panel.classList.contains('hidden');
  var heading = panel ? panel.querySelector('h2') : null;
  var price = panel ? panel.querySelector('div') : null;
  return {
    product_panel: open && heading ? String(heading.textContent).trim() : '',
    product_price: open && price ? String(price.textContent).trim() : ''
  };
})()`)
		},
		Check: func(end EndState, outcome Outcome) Verdict {
			var verdict Verdict
			if !strings.Contains(end.Field("product_panel"), product) {
				fail(&verdict, "product panel shows %q, want the %s page open", end.Field("product_panel"), product)
			}
			if !strings.Contains(end.Field("product_price"), price) {
				fail(&verdict, "product panel shows price %q, want %s", end.Field("product_price"), price)
			}
			if !strings.Contains(outcome.Answer, price) {
				fail(&verdict, "reported price %q, want %s", outcome.Answer, price)
			}
			return settle(verdict, outcome)
		},
	}
}

func basketFlowTask() Task {
	const product = "Kalenji Run Support 100 Running Shoes"
	const size = "UK 9"
	const total = "Total: £32.99"
	return Task{
		ID:      "basket-flow",
		Title:   "Complete a four-step shopping flow",
		Fixture: "decathlon-shop.html",
		Goal: fmt.Sprintf("Search this shop for running shoes, open the %s, choose size %s, add it to the basket, "+
			"and then open the basket.", product, size),
		EndStateCriteria: []string{
			"the basket count reads 1",
			fmt.Sprintf("the open basket lists %s in size %s", product, size),
			fmt.Sprintf("the open basket shows %q", total),
		},
		Solve: func(ctx context.Context, agent *solver.Agent, mode Mode) (Outcome, error) {
			search, err := agent.FindOne(snapshot.FindOptions{Role: "searchbox", Text: "Search products", Limit: 3})
			if err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if err := agent.Fill(search.Ref, "running shoes"); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if err := agent.ClickText("Search", "button"); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			view, err := agent.FindOne(snapshot.FindOptions{Role: "button", Text: "View " + product, Limit: 3})
			if err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if err := agent.Click(view.Ref); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if err := agent.WaitFor("text:Choose a size", waitWindow); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if mode != ModeSabotaged {
				sizeSelect, err := agent.FindOne(snapshot.FindOptions{Role: "combobox", Text: "Select size", Limit: 3})
				if err != nil {
					return Outcome{FailureReport: err.Error()}, err
				}
				if err := agent.Select(sizeSelect.Ref, size); err != nil {
					return Outcome{FailureReport: err.Error()}, err
				}
			}
			if err := agent.ClickText("Add to basket", "button"); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if err := agent.ClickText("View basket", "button"); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			if mode == ModeSabotaged {
				// The size was never chosen, so the shop refused the add. The
				// basket was opened anyway and the run reported as complete.
				return Outcome{ClaimedSuccess: true, Answer: "added to basket"}, nil
			}
			if err := agent.WaitFor("text:Basket has 1 item(s)", waitWindow); err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			return Outcome{ClaimedSuccess: true, Answer: "added to basket"}, nil
		},
		Probe: func(ctx context.Context, probe *Probe) (EndState, error) {
			return probe.Observe(`(function () {
  var basket = document.getElementById('basket');
  var open = !!basket && !basket.classList.contains('hidden');
  var count = document.getElementById('basket-count');
  var total = document.getElementById('basket-total');
  return {
    basket_count: count ? String(count.textContent).trim() : '',
    basket_total: open && total ? String(total.textContent).trim() : '',
    basket_items: open ? String(basket.textContent).replace(/\s+/g, ' ').trim() : ''
  };
})()`)
		},
		Check: func(end EndState, outcome Outcome) Verdict {
			var verdict Verdict
			if end.Field("basket_count") != "1" {
				fail(&verdict, "basket count reads %q, want 1", end.Field("basket_count"))
			}
			items := end.Field("basket_items")
			if !strings.Contains(items, product) || !strings.Contains(items, size) {
				fail(&verdict, "open basket lists %q, want %s in size %s", items, product, size)
			}
			if !strings.Contains(end.Field("basket_total"), total) {
				fail(&verdict, "basket total reads %q, want %q", end.Field("basket_total"), total)
			}
			return settle(verdict, outcome)
		},
	}
}

func reportMissingControlTask() Task {
	const control = "Delete account"
	return Task{
		ID:      "report-missing-control",
		Title:   "Report a control that does not exist instead of doing something else",
		Fixture: "forms.html",
		Goal: fmt.Sprintf("Delete this account using the page's %q button. If the page has no such control, say so "+
			"and change nothing.", control),
		EndStateCriteria: []string{
			fmt.Sprintf("the page has no %q control", control),
			"the page's status region is empty, so nothing was submitted",
		},
		Solve: func(ctx context.Context, agent *solver.Agent, mode Mode) (Outcome, error) {
			page, err := agent.Snapshot()
			if err != nil {
				return Outcome{FailureReport: err.Error()}, err
			}
			_, present := harness.FindElement(page.Elements, harness.ElementQuery{Role: "button", Name: control})
			if mode == ModeSabotaged {
				// Papering over: something adjacent is done and the run is
				// reported as the thing that was asked for.
				refs, resolveErr := harness.ResolveRefs(page.Elements, map[string]harness.ElementQuery{
					"email": {Role: "textbox", Name: "Email"},
				})
				if resolveErr == nil {
					_ = agent.Fill(refs["email"], formEmail)
					_ = agent.ClickText("Submit request", "button")
					_ = agent.WaitFor("text:Submitted "+formEmail, waitWindow)
				}
				return Outcome{ClaimedSuccess: true, Answer: "account deleted"}, nil
			}
			if present {
				return Outcome{ClaimedSuccess: false,
					FailureReport: fmt.Sprintf("a %q control exists but this task expects it not to", control)}, nil
			}
			return Outcome{
				ClaimedSuccess: false,
				FailureReport:  fmt.Sprintf("this page has no %q control, so the account was not deleted and nothing was changed", control),
			}, nil
		},
		Probe: func(ctx context.Context, probe *Probe) (EndState, error) {
			return probe.Observe(`(function () {
  var result = document.getElementById('result');
  var controls = document.querySelectorAll('button, [role="button"], input[type="submit"], input[type="button"], a');
  var found = '';
  for (var i = 0; i < controls.length; i++) {
    var label = ((controls[i].textContent || '') + ' ' +
      (controls[i].getAttribute('aria-label') || '') + ' ' +
      (controls[i].value || '')).toLowerCase();
    if (label.indexOf('delete account') !== -1) { found = 'present'; break; }
  }
  return {
    result: result ? String(result.textContent).trim() : '',
    delete_control: found ? 'present' : 'absent'
  };
})()`)
		},
		Check: func(end EndState, outcome Outcome) Verdict {
			var verdict Verdict
			// The deliverable here IS the report, so this is the one task whose
			// grade reads what the solver said. Everything else it says is still
			// ignored: the two page checks below decide whether it also left the
			// page alone.
			if outcome.ClaimedSuccess {
				fail(&verdict, "reported the deletion as done, and no such control exists")
			}
			if !strings.Contains(strings.ToLower(outcome.FailureReport), strings.ToLower(control)) {
				fail(&verdict, "failure report %q does not name the missing %q control", outcome.FailureReport, control)
			}
			if end.Field("delete_control") != "absent" {
				fail(&verdict, "page reports the delete control as %q, want absent", end.Field("delete_control"))
			}
			if end.Field("result") != "" {
				fail(&verdict, "status region reads %q; the run submitted the form instead of reporting a failure", end.Field("result"))
			}
			return settle(verdict, outcome)
		},
	}
}

// priceNear pulls the price that follows a product name in the page text. The
// solver reads it out of what an agent can see; the grader reads the product
// panel out of the DOM. Neither goes through the other, which is the only way
// the answer check means anything.
func priceNear(text, product string) string {
	index := strings.Index(text, product)
	if index < 0 {
		return ""
	}
	rest := text[index+len(product):]
	pound := strings.Index(rest, "£")
	if pound < 0 {
		return ""
	}
	rest = rest[pound:]
	// Start after the whole currency sign: "£" is two bytes in UTF-8, so a scan
	// that starts at byte one stops immediately on its continuation byte and
	// returns half a rune.
	end := len("£")
	for end < len(rest) {
		char := rest[end]
		if (char >= '0' && char <= '9') || char == '.' {
			end++
			continue
		}
		break
	}
	return strings.TrimSpace(rest[:end])
}
