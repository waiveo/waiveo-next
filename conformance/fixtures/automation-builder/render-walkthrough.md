# Automation-builder fixture — render walkthrough

This walks `automation-builder.json` top to bottom against a hypothetical bound
`automations` resource (`sample-data.json`'s one row, shaped exactly like
`api/openapi.yaml`'s `Automation` schema) and its `$context.presets`
collection, asserting for every widget and binding construct the fixture uses
that it is defined in `contracts/ui-schema-1.md`. `node
conformance/fixtures/fixture-lint.mjs` performs the same check mechanically
(zero undefined references, confirmed — see its output at the bottom); this
document is the human-readable walk of *why* each piece is there, which the
lint script cannot assert on its own.

The document declares `pageType: "list-detail"` (UIS-003) with a `context`
map (UIS-004) naming the `presets` collection (UIS-150) and one recursive
`fragments` entry, `condition-editor` (UIS-004, UIS-182).

## List panel

`list.source = "automations"` (UIS-020) is the outermost array; `list.display`
is a `table` (UIS-070) bound to that same array, its three `columns[].cell`
BindingExprs (UIS-071, table cells are per-row-scoped like `repeat`) covering:

- `item.name` — a bare Binding.
- `{"compute": "label", "args": ["rules/1:mode", "item.mode"]}` — a `label`
  Computed (UIS-140) resolving `item.mode`'s value against the `rules/1:mode`
  vocabRef's label map. That label map exists on this same page (the mode
  `select` in the detail panel, below) exactly as UIS-140's `label` entry
  requires ("a page with no such declaration... MUST declare one... this
  contract does not carry a second, parallel label registry").
- `{"compute": "count", "args": ["item.triggers"]}` — a `count` Computed.

`table`'s `on.rowPress` (UIS-070) fires a `set` ActionRef (UIS-160):
`{"verb": "set", "target": "$ui.selected", "value": "item.id"}` — pressing
the sample row writes its `id`
(`01J8Z3K4N5P6Q7R8S9T0V1W2A1`) into ephemeral UI state (UIS-104).

`detail.source = "automations[id=$ui.selected]"` (UIS-020) is a predicate-index
Binding (UIS-101): with `$ui.selected` set as above, it resolves to the sample
automation record, which becomes `detail.root`'s Scope (UIS-005, UIS-103).
`detail.emptyMsg` (UIS-020) covers the no-selection case.

`newAction` (UIS-021) is a `create` ActionRef targeting the `automations`
collection, seeded with a minimally-valid draft (`mode: "single"`, one
placeholder `state` trigger, one placeholder `device_command` action) so the
"New" affordance and the Save flow described below are the same code path.

## Detail panel — name, enabled, trigger count

Three widgets read/write directly against the selected record's Scope:

- `text-input` (UIS-070) bound to `name` — read-write per UIS-065.
- `toggle` (UIS-070) bound to `enabled`.
- `badge` (UIS-070) whose `value` is `{"compute": "count", "args":
  ["triggers"]}` — for the sample record, resolves to `5`.

## Triggers section — five trigger kinds (floor: ≥4)

A `section` (UIS-070, `collapsible: true`) contains a `repeat` (UIS-070) bound
to `triggers`. Each item's `itemTemplate` establishes `item` as that trigger's
own Scope (UIS-107) and renders:

1. A `select` (UIS-070) bound to `item.type`, `options` a `vocab`-kind
   OptionSource (UIS-132) referencing `rules/1:trigger-kind` (UIS-120) with a
   complete `labels` map (all 8 members, satisfying UIS-132's "omitting any
   member... MUST fail" floor even though only 5 are wired to `switch` cases
   below — UIS-122 explicitly allows a vocabRef's OptionSource to stay
   complete while a page's own `switch` only handles a subset).
2. A `switch` (UIS-070) whose `discriminant` is the bare Binding `item.type`
   (a BindingExpr per UIS-141), with one `case` per trigger kind:

   | `when` | rendered fields | widgets used | sample value exercised |
   |---|---|---|---|
   | `state` | entity, `to` (multi-value), `for` | `entity-picker`, `multi-select` (`literal` OptionSource, UIS-131), `duration-input` | `to: ["off","standby"]`, `for: 300` |
   | `numeric` | entity (`modes` restricted to `["entity","selector"]`, proving UIS-073's `modes` prop is live), `attribute`, `above`, `for` | `entity-picker`, `text-input`, `number-input`, `duration-input` | `attribute: "cpu_temp"`, `above: 80` |
   | `time` | `at`, `misfire` | `time-of-day`, `select` (`vocab`-kind, `rules/1:misfire`) | `at: "22:00:00"`, `misfire: "catch_up_once"` |
   | `sun` | `event`, `offset` | `select` (`literal`-kind: `sunrise`/`sunset`), `number-input` | `event: "sunset"`, `offset: -900` |
   | `template` | `expression.expr` | `text-input` (`multiline: true`) | an `{expr: "..."}` pipeline string (UIS-123: presented as opaque string data, never parsed by this contract's own grammar) |

   Five kinds — state, numeric, time, sun, template — exceed the fixture's
   ≥4-kind floor, and deliberately include one edge-class kind with `for:`
   (state), one with both `for:` and an `attribute` (numeric), and one
   app-coupled kind requiring free-text expression entry (template), so the
   binding grammar is proven against more than the easy cases.
3. A `button` (UIS-070, `style: "destructive"`) whose required `on.press`
   (UIS-074) is `{"verb": "repeat-remove", "target": "item"}` (UIS-160,
   UIS-162) — self-targeting: `item` is the innermost repeat item (this
   trigger), and the renderer removes exactly the element it is rendering,
   inferring the array and index from the item's own iteration context. No
   array path or `$index` is supplied (UIS-107 provides no parent-array
   token; UIS-162 makes removal an item self-reference precisely so it needs
   none).

Outside the `repeat`, one `button` fires `{"verb": "repeat-add", "target":
"triggers", "itemDefault": {"type": "state"}}` (UIS-160, UIS-162) to append a
blank state trigger — `repeat-add`'s `target` IS an ordinary array Binding,
resolved here in the outer (section) scope where `triggers` is directly
addressable, the asymmetry with `repeat-remove` that UIS-162 states.

## Conditions section — nested and/or/not groups

This is the fixture's hardest stress test, and the one that forced two
mid-draft revisions to the contract (below). `rules/1`'s own Condition wire
shape (RUL-100) is a **key-presence** discriminated union — a composition
carries a top-level `and`/`or`/`not` key and no `type` key at all, while a
leaf carries `type` and no composition key — not a single shared field whose
*value* varies. The `condition-editor` fragment (UIS-004, declared once,
referenced recursively) handles this with:

```json
"discriminant": { "compute": "firstKey", "args": ["item", ["and", "or", "not", "type"]] }
```

— a `firstKey` Computed (UIS-140, added specifically for this) feeding a
`switch` whose `discriminant` is a BindingExpr rather than a bare Binding
(UIS-070, widened for this same reason; UIS-142 states the rationale
normatively). Sample data: `conditions[0]` is `{"and": [<leaf>, {"or":
[<leaf>, {"not": <leaf>}]}]}` — three levels deep, exercising `and`, `or`,
`not`, and a leaf all in one tree.

Per `switch` case:

- **`and`** — a `section` titled via `titleMsg`, containing a `repeat` bound
  to `item.and` whose `itemTemplate` wraps a scope-transparent, self-referential
  `fragment` reference (`{"type": "fragment", "props": {"ref":
  "condition-editor"}}`, UIS-070, UIS-181, UIS-183 — no `bind`, so `item`
  inside the recursive call is the *inner* repeat's own element, correctly
  shadowing the outer `item` per UIS-183's transparency rule) plus a
  `repeat-remove` button whose `target` is the innermost `item` (UIS-160/162):
  the button sits inside the `item.and` repeat's `itemTemplate`, where `item`
  is the sub-condition being edited, and the renderer removes exactly that
  rendered element from the array its enclosing `repeat` iterates — no array
  path is named (and none *could* be, since the inner `item` shadows the outer,
  UIS-107, making the `and` array unaddressable from here). Two distinct `Add`
  buttons follow, placed as *siblings* of the repeat (outer scope, where
  `item.and` correctly names the `and` array): `repeat-add` on `item.and` with
  `itemDefault: {"type": "state"}` (add a leaf) and a second with
  `itemDefault: {"or": []}` (add a nested group) — proving `repeat`'s
  deliberate lack of an intrinsic add/remove affordance (Widget catalog,
  UIS-070's `repeat` row has no `addable`/`removable` prop) was the right call:
  a single boolean could not express two differently-shaped default items on
  the same array.
- **`or`** — the mirror of `and`, on `item.or`.
- **`not`** — a `fragment` reference *with* `bind: "item.not"` (UIS-183's
  non-transparent form — `not` wraps exactly one nested condition, not an
  array, so this establishes a new Scope directly at that single nested
  object rather than iterating).
- **`type`** — a second, nested `switch` (`discriminant: "item.type"`, an
  ordinary bare Binding this time — leaf conditions really do share one
  value-typed field) with cases `state`, `numeric`, `variable`, `template`,
  rendering `entity-picker`+`multi-select`, `entity-picker`+two
  `number-input`s, `text-input`+`number-input`, and a `text-input` on
  `item.expression.expr` respectively.

At the top level, `detail.root`'s conditions section repeats this same
wrapper pattern once more (`repeat` bound to `conditions`, `fragment` ref with
no `bind`, and a `repeat-remove` button self-targeting the innermost `item`
exactly as the nested case does), so the recursion described above is entered
uniformly whether a condition is top-level or nested — and removal works
identically at every depth precisely because it never names the array.

## Actions section — preset-batch selection (+ 3 more kinds)

A `repeat` (UIS-070) bound to `actions`. Each item renders a `select` bound to
`item.type` (`vocab`-kind, `rules/1:action-kind`, all 9 members labeled) and a
`switch` on the same `item.type`:

| `when` | widgets | sample value |
|---|---|---|
| `device_command` | `entity-picker`, `text-input` (command name) | `command: "power"` |
| `preset_batch` | `select`, `options` a **`data`-kind** OptionSource (UIS-133): `{"kind": "data", "source": "$context.presets", "valuePath": "id", "labelPath": "name"}` | `preset_id` resolves to the sample `"Evening scene"` row of `$context.presets` |
| `delay` | `duration-input` bound to `item.duration_seconds` | `30` |
| `log` | `text-input` (`multiline`) bound to `item.message` (a literal string is itself a valid `rules/1` Expression, RUL-280 — no `.expr` wrapper needed here, unlike the template-trigger/template-condition cases above) | `"Lobby wind-down complete"` |

`preset_batch` is the fixture's required proof that a picker can be populated
from the operator's *actual* configured data (not a closed enum) — the
`data`-kind OptionSource (UIS-133) reads `$context.presets` (UIS-105,
UIS-150), which is declared once at the document root and resolves, per
UIS-151, to exactly the array shape a `table`/`repeat`'s own `source` binding
already consumes elsewhere in this same document — one array shape, three
different consumers (`table.source`, `repeat.bind`, `OptionSource.data.source`).

Each item also carries a `repeat-remove` button self-targeting `item` (the
current action), and one `Add action` button outside the `repeat`
(`repeat-add target: "actions"`, `itemDefault: {"type": "device_command"}`).

## Mode section — mode selector + conditionally-visible `max` + `for:`/duration

- `select` bound to `mode`, `vocab`-kind OptionSource on `rules/1:mode` (all 4
  members labeled — `single`/`restart`/`queued`/`parallel`). Sample value:
  `"parallel"`.
- `number-input` bound to `max`, `visibleIf: {"compute": "eq", "args":
  ["mode", {"lit": "parallel"}]}` (UIS-063, UIS-140's `eq`, UIS-108) — visible
  only when the sibling `mode` field equals the string literal `"parallel"`.
  The literal MUST be `{"lit": "parallel"}`, not a bare `"parallel"`: under
  UIS-108 a bare string in a Computed arg is a *Binding* (it would resolve
  `parallel` as a field on the record, find nothing, and make `visibleIf`
  always false — the exact trap the disambiguation rule exists to close).
  `"mode"`, by contrast, IS a bare-string Binding here, correctly reading the
  sibling field's value. This mirrors `rules/1` RUL-244's own
  "`max`... meaningful only under... `parallel`" rule and its
  `MODE_MAX_NOT_APPLICABLE` compile error at the *rules* layer with a UI that
  never lets the user populate `max` under a mode where it would be rejected.
  Sample value: `max: 3`. UIS-072's nullable-write rule is exercised
  conceptually here too — clearing this field writes `null`, matching
  `Automation.max`'s `["integer", "null"]` OpenAPI type exactly.
- `for:` and duration inputs appear throughout the triggers section above
  (`duration-input` on `item.for` for both `state` and `numeric` kinds) and
  the actions section (`duration-input` on `item.duration_seconds` for
  `delay`) — both always whole seconds on the wire per UIS-070's `duration-input`
  bind-shape, matching `rules/1` RUL-024/RUL-190 exactly.

## Save

A `button` (`style: "primary"`) with `on.press: {"verb": "submit"}` (UIS-160,
UIS-161) persists the whole edited record — `submit`'s default `target` (the
page's own primary bound resource, i.e. whatever `detail.source` currently
resolves to) needs no explicit `target` field.

## Fixture-lint result

```
$ node conformance/fixtures/fixture-lint.mjs
fixture-lint: OK (automation-builder) — 15 widget type(s) used [badge, button, duration-input,
entity-picker, fragment, multi-select, number-input, repeat, section, select, switch, table,
text-input, time-of-day, toggle], 71 binding(s) checked, 4 vocabRef(s) used [rules/1:action-kind,
rules/1:misfire, rules/1:mode, rules/1:trigger-kind], 5 action verb(s) used [create, repeat-add,
repeat-remove, set, submit], 4 compute fn(s) used [count, eq, firstKey, label], 3 option kind(s)
used [data, literal, vocab], 1 fragment ref(s) used [condition-editor] — zero undefined references
SUMMARY: fixture-lint: OK (0 undefined references)
```

`fixture-lint` checks binding-string *syntax* and closed-vocabulary reference
closure — it deliberately does NOT model Scope resolution (which array a given
`item` binds to at a given nesting depth). That semantic layer was verified
separately by a throwaway resolver run against `sample-data.json`, confirming:
`detail.source`'s predicate-index binding resolves to the selected record; the
mode/`max` `visibleIf` `eq(mode, {lit:"parallel"})` yields `true` for the
sample's `parallel` mode; and a nested-condition-group `repeat-remove`
self-targeting `item` removes exactly the intended element from the correct
inner array (while the pre-fix `target: "item.or"` form resolves to
`undefined`, confirming the gap the fix closes).

Widgets defined in the catalog but **not** exercised by this fixture: `slot`,
`text`, `stat-tile` — all three are dashboard/cross-pack-composition oriented
and are instead exercised by the smaller page-type-specific corpus cases in
`conformance/corpora/ui-schema-1/` (UIS-070's catalog does not require every
member to appear in every conformant document; UIS-134's "declared-but-unused
is not an error" convention, borrowed from `manifest/1` MAN-032, covers this
directly).

## Gaps this fixture surfaced (closed in the same task, not deferred)

1. **`switch.discriminant` typed as a bare `Binding`** could not express
   `rules/1`'s key-presence Condition union. Fixed by widening it to
   `BindingExpr` (UIS-070) and adding the `firstKey` Computed (UIS-140,
   UIS-142) — discovered while drafting the conditions section above, before
   any corpus or lint work began.
2. **`repeat`'s originally-planned intrinsic `addable`/`removable` props**
   could not express "Add condition" vs. "Add group" as two differently-shaped
   defaults on the same array. Resolved by never adding those props in the
   first place — composing `repeat-add`/`repeat-remove` ActionRef verbs with
   explicit `Button`s instead (UIS-160) — confirmed necessary by this
   fixture's conditions section specifically.
3. **`repeat-remove` could not name its own enclosing array from inside a
   `repeat` template.** A first attempt targeted the array by path (`target:
   "item.and"`, `at: "item.$index"`) from a remove button *inside* the
   `itemTemplate` — but the inner `item` shadows the outer (UIS-107), so
   `item.and` reads `.and` off the wrong object (an array *element*, a leaf),
   resolving to `undefined`; and there is no ancestor/parent-array token in the
   grammar to escape the shadow. An adversarial review re-derived this by hand
   against the sample data and confirmed it was structurally unfixable at the
   authoring layer — a real contract gap, not a wiring typo. **Closed by
   redefining `repeat-remove`** (UIS-160/162): its `target` is now an itemScope
   self-reference (`item`), and the renderer removes exactly the rendered
   element, inferring the (array, index) from the item's own iteration context.
   The array is never named — which is precisely what makes removal work
   identically at any nesting depth. (An earlier draft of this walkthrough
   claimed placing the button "at each repeat's call site" solved it; that was
   wrong — the button MUST be inside the template to know which item, and the
   self-reference is what actually resolves the tension.)
4. **A Computed's generic string arg was ambiguous between a literal and a
   Binding.** `{"compute": "eq", "args": ["mode", "parallel"]}` (the mode/`max`
   `visibleIf`) has no disambiguation rule as first drafted — if `"parallel"`
   resolves as a Binding (a field lookup), it finds nothing and `visibleIf` is
   always false, silently breaking the conditionally-visible `max` field. The
   same latent ambiguity sat in predicate values and `set.value`. **Closed by
   UIS-108**: a uniform JSON-type disambiguation (number/boolean/null = literal;
   bare string = Binding; `{"lit": …}` = explicit literal escape hatch;
   `{"compute": …}` = Computed) applied to every literal-or-Binding position,
   with the fixture's `eq` now written `["mode", {"lit": "parallel"}]`. Also
   surfaced by the same adversarial review.
