# Operator approvals

With daemon approvals enabled, a gated call returns `approval_required` and an
`approval_id` immediately. Preserve its exact original tool and arguments and
continue independent work while the operator reviews the inbox. Check
`brw_approval_status({approval_id})`; when approved, use
`brw_approval_resume({approval_id,tool,arguments})` with that original argument
object. You can also retry the original tool with unchanged arguments plus
`approval_id`. Neither tool grants approval. Never request or pass the operator
token, or open the inbox in the controlled browser.

Approvals expire after ten minutes and bind the action to its tab, session and
observed page state. A changed binding needs a new request. A consumed approval
cannot be replayed; it does not prove the backend transaction succeeded. If an
outcome is uncertain, inspect it and involve the operator before taking another
consequential action. Split gated mutations into single visible actions:
mutating batches, plans and recipes require splitting or human takeover.
Ordinary read calls do not capture approval evidence or access its store.
