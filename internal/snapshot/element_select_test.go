package snapshot

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// This fixture mirrors the accessibility-hostile shape used by Element UI 2.x:
// a readonly input inside .el-select and plain <li> options whose Vue listeners
// do not appear as onclick attributes or ARIA roles in the DOM.
const elementSelectFixture = `<!doctype html>
<style>
  .el-select { width: 260px; padding: 8px; border: 1px solid #888; }
  .el-input__inner { width: 230px; height: 32px; }
  .el-select-dropdown { width: 260px; border: 1px solid #888; }
  .el-select-dropdown__item { display: block; height: 30px; line-height: 30px; padding: 0 8px; }
</style>
<div class="el-select" id="select">
  <div class="el-input"><input class="el-input__inner" readonly placeholder="Choose client"></div>
</div>
<div class="el-select-dropdown" id="menu" style="display:none">
  <ul class="el-select-dropdown__list">
    <li class="el-select-dropdown__item">Alpha client</li>
    <li class="el-select-dropdown__item">Beta client</li>
    <li class="el-select-dropdown__item is-disabled">Disabled client</li>
  </ul>
</div>
<script>
  var select = document.getElementById('select');
  var menu = document.getElementById('menu');
  var input = select.querySelector('input');
  select.addEventListener('click', function() { menu.style.display = menu.style.display === 'none' ? 'block' : 'none'; });
  menu.querySelectorAll('.el-select-dropdown__item').forEach(function(item) {
    item.addEventListener('click', function(event) {
      event.stopPropagation();
      if (item.classList.contains('is-disabled')) return;
      input.value = item.textContent.trim();
      menu.style.display = 'none';
    });
  });
</script>`

func TestElementUISelectIsSemanticAndClickable(t *testing.T) {
	ctx, cancel := newHeadlessCtx(t)
	defer cancel()
	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()
	target := "data:text/html," + url.PathEscape(elementSelectFixture)
	if err := chromedp.Run(runCtx, chromedp.Navigate(target)); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	comboSnap, err := EvaluateWithOptions(runCtx, SnapshotOptions{Role: "combobox", Limit: 10, ViewportOnly: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(comboSnap.Elements) != 1 || comboSnap.Elements[0].Tag != "input" || comboSnap.Elements[0].Name != "Choose client" {
		t.Fatalf("Element UI input was not exposed as a combobox: %+v", comboSnap.Elements)
	}
	combo := comboSnap.Elements[0]
	box, err := ResolveOrRecoverBox(runCtx, combo.Ref)
	if err != nil {
		t.Fatal(err)
	}
	clicked, err := ClickXY(runCtx, box.ViewportX, box.ViewportY)
	if err != nil || !clicked.OK {
		t.Fatalf("open Element UI select: result=%+v err=%v", clicked, err)
	}

	optionSnap, err := EvaluateWithOptions(runCtx, SnapshotOptions{Role: "option", Limit: 10, ViewportOnly: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(optionSnap.Elements) != 3 {
		t.Fatalf("plain Element UI option <li>s were not exposed: %+v", optionSnap.Elements)
	}
	if !optionSnap.Elements[2].Disabled {
		t.Fatalf("is-disabled option not marked disabled: %+v", optionSnap.Elements[2])
	}

	selected, err := ClickText(runCtx, ClickTextOptions{Text: "Beta client", Role: "option", Exact: true})
	if err != nil || !selected.OK {
		t.Fatalf("click Element UI option: result=%+v err=%v", selected, err)
	}
	var value string
	if err := chromedp.Run(runCtx, chromedp.Value("input.el-input__inner", &value)); err != nil {
		t.Fatal(err)
	}
	if value != "Beta client" {
		t.Fatalf("selected value = %q, want Beta client", value)
	}
}
