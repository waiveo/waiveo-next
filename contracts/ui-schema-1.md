# Declarative UI

**Contract:** ui-schema/1
**Version:** 1.1
**Status:** review

## Scope

ui-schema/1 defines the declarative document a pack ships for each page it contributes (`manifest/1` MAN-060): the closed set of page types a document may declare, the closed catalog of widgets a page's content is built from, the closed grammar a widget uses to bind to data, and how a document composes reusable fragments and exposes slots for other packs' fragments. A host renders a conformant document directly — no pack-authored rendering code ever runs, mirroring `manifest/1`'s own "entirely before any pack code runs" posture for every other declarative surface.

- In scope: the page-document envelope and its file-location convention; the four page types and their structural schemas, including a keyset-cursor paginated form for a `list-detail` page's list binding; the widget-node envelope; the enumerated widget catalog (props — including every interactive widget's required accessible label, UIS-075, an action control's own bindable disabled state, UIS-076, and a text node's live-region announcement, UIS-077 — bindings, events per widget); the binding grammar (data paths, local-scope rules, vocabulary references, option sources, computed values — including locale-formatted date/number rendering, UIS-143 — and an optional live-updating flag, delivered over `events/1`, UIS-109–110); page-scoped ephemeral UI state; context feeds; the closed action-verb grammar a widget event invokes, together with the two verb-independent envelope fields that gate an invocation behind an operator confirmation (UIS-165) and publish its asynchronous outcome — pending, succeeded, or refused with a code — into page-scoped state a widget can bind (UIS-166–167); fragment declaration, reference, and recursion; the `Slot` widget; the validation story (closed-set and grammar-well-formedness checks) and this contract's error taxonomy.
- Out of scope: the manifest fields that declare a page's route, title, and slot-contribution eligibility (`manifest/1` MAN-060–062) — this contract defines the page document a `path` resolves to and the `Slot` widget's own rendering behavior, not the pack-manifest fields that register a page or wire a fragment page into a target slot; the automation rule vocabulary itself — triggers, conditions, actions, modes, the Expression grammar (`rules/1`), referenced here only by name as the closed source a vocabulary-reference binding points at; the platform's label-selector grammar's own syntax (`api/1` API-040–046), referenced here only by name; the device-class registry's own state/attribute/command content, referenced here only by name; the pack action-dispatch mechanism a `call-action` verb ultimately resolves to (`ctx/1` CTX-110); the rendering engine's own implementation (a host concern, not a wire or document contract); the `msg:` locale-catalog mechanism itself (`manifest/1` MAN-003/MAN-111), reused here by reference.

## Definitions

- **Page document** — the JSON document this contract governs: one `pageType` plus that type's own structural fields, optionally declaring `fragments` and `context` (Page documents).
- **Widget node** — a single element of a page's content tree: `{type, id?, bind?, props?, on?, visibleIf?, children?}` (Widget node envelope), where `type` is a member of the closed Widget catalog.
- **Kebab identifier** — a string matching `^[a-z][a-z0-9-]*$`: lowercase ASCII letters, digits, and hyphens, starting with a letter. This contract's naming convention for widget `type` values, page-type names, fragment names, context-feed names, and vocabulary-reference and computed-function names.
- **Binding** — a string in this contract's data-path grammar (Binding grammar: data paths), naming the location a widget reads (and, for an input-category widget, writes) relative to its enclosing scope.
- **LiveBinding** — `{path, live: true}`, the live-updating alternate a Binding-typed position MAY use instead of a bare string (Binding grammar: data paths, UIS-109), re-evaluated over `events/1` for as long as it is mounted rather than fetched once (UIS-110).
- **Scope** — the data path a widget node's own `bind` (and its descendants' relative bindings) resolve against; established by the page root, narrowed by `repeat` per rendered item, and narrowed or left transparent by `fragment` (Binding grammar: data paths).
- **Computed** — an object `{compute, args}` drawn from this contract's closed view-computation function list (Binding grammar: computed values), evaluated over already-bound data; a separate, smaller, differently-scoped grammar from `rules/1`'s own Expression grammar (RUL-280) — never a redefinition of it.
- **BindingExpr** — a Binding, a Computed, or a JSON literal, used wherever a widget needs a computed-or-bound value (`visibleIf`, a step's `canAdvanceIf`, a `text`/`badge`/`stat-tile` widget's `value`, a `table` column's `cell`).
- **Vocabulary reference (vocabRef)** — a closed, namespaced string (Binding grammar: vocabulary references) naming one of `rules/1`'s own closed enumerations, usable as an `options` source so a picker widget can offer that vocabulary's members without this contract restating them.
- **OptionSource** — the closed grammar (Binding grammar: option sources) a `select`/`multi-select` widget's `options` prop uses to describe its candidate values: `literal`, `vocab`, or `data`.
- **Context feed** — a page-declared, read-only, named data source (Context feeds) reachable via `$context.<name>`, distinct from the page's own primary bound resource.
- **Ephemeral UI state (`$ui`)** — page-scoped, renderer-held, never-persisted binding state (Binding grammar: data paths) a page uses for pure interaction bookkeeping (e.g. which row is selected).
- **ActionRef** — a closed-verb object `{verb, ...fields}` (Actions) a widget event (`on.<event>`) invokes. Two of its fields are **envelope** fields, valid alongside any verb rather than belonging to one: `confirm` (a ConfirmSpec, UIS-165) and `outcomeTo` (UIS-166).
- **ConfirmSpec** — `{titleMsg, bodyMsg?, confirmLabelMsg?, cancelLabelMsg?, destructive?}` (Actions, UIS-165), the operator acknowledgement an ActionRef's `confirm` field interposes between a widget event and that ActionRef's own dispatch.
- **ActionOutcome** — `{status, code?, detail?, traceId?, result?}` (Actions, UIS-166), the renderer-written record an ActionRef's `outcomeTo` field publishes into `$ui` across one invocation's lifecycle: `status` one of `pending`, `ok`, `error`.
- **Fragment** — a named, reusable widget-node subtree declared in a page document's `fragments` map, inserted via a `fragment` widget node (Fragments & slots).
- **Slot** — a named insertion point (the `slot` widget) where a host renders another pack's fragment-typed page, per `manifest/1`'s own slot-acceptance rules (MAN-062).

## Normative requirements

### Page documents

**[UIS-001]** A pack's ui-schema/1 page document for a `manifest/1` `ui.pages[]` entry (MAN-060) MUST be bundled inside the pack artifact at `ui/<path>.json`, where `<path>` is that entry's own `path` field with each `/`-separated segment mapped to a nested path component (e.g. an entry with `path: "settings/general"` bundles at `ui/settings/general.json`).

**[UIS-002]** This contract's Page types (below) constitute exactly the page-type registry `manifest/1` MAN-010 (`compat.renderer`) and MAN-060 (`pageType`) validate against; a host implementing ui-schema/1 v1 populates that registry with precisely the page-type names enumerated below, no more and no fewer. Growth of the page-type set is exclusively a ui-schema/1 minor-version change (Negotiation), the same discipline `rules/1` RUL-001 applies to its own vocabulary.

**[UIS-003]** A page document MUST declare `pageType` as exactly one of `list-detail`, `settings-form`, `dashboard`, `wizard` (Page types). A document whose `pageType` is not a member of this set MUST fail validation as `UNKNOWN_PAGE_TYPE` — the same code `manifest/1` already defines for the corresponding manifest-side check, reused here rather than duplicated (Error taxonomy).

**[UIS-004]** A page document MAY declare `fragments` as an object mapping each key (a Kebab identifier) to a widget node — that fragment's own root (Fragments & slots). A page document MAY declare `context` as an object mapping each key (a Kebab identifier) to a ContextRef (Context feeds).

**[UIS-005]** A handful of top-level page-document fields are **root Bindings**: resolved against an implicit page-level resource namespace rather than against any enclosing Scope, because none exists yet at that point — `list.source` and `detail.source` (`list-detail`), `source` (`settings-form`), each tile's own top-level `bind`/BindingExpr props (`dashboard`), and `draftSource` (`wizard`, when declared). A root Binding's own **resolved value** becomes the outermost Scope for whatever widget-node subtree it roots (`detail.root` for `list-detail`'s `detail.source`, `sections[].fields` for `settings-form`'s `source`, a wizard step's shared draft scope for `draftSource`) — the Binding itself is never "inside" the Scope it establishes. A Binding with no reserved prefix (Binding grammar: data paths) resolves relative to the innermost enclosing Scope in effect at that widget node; `list.source`, having no widget-node subtree of its own (it feeds `list.display` directly as an array, Page types: list-detail), is a root Binding without also becoming a Scope.

### Page types: list-detail

**[UIS-020]** A `list-detail` page MUST declare `list` as `{source, display}` and `detail` as `{source, root, emptyMsg?}`. `list.source` and `detail.source` are each root Bindings (UIS-005) to an array and to a single record respectively; `list.display` is a widget node (typically `table`, Widget catalog) rendered against `list.source`'s array. `detail.source`'s resolved record becomes `detail.root`'s outermost Scope (UIS-005); `detail.root` is a widget node. `detail.emptyMsg`, when declared, is a `msg:` reference shown in place of `detail.root` while `detail.source` resolves to absent/null.

**[UIS-021]** A `list-detail` page MAY declare `newAction` as an ActionRef (Actions), typically `create` (UIS-160, UIS-161), invoked by a host-rendered "new" affordance separate from `list.display`'s own content.

**[UIS-022]** `detail.source` conventionally composes a predicate-index path segment (Binding grammar: data paths) against `list.source`'s own array, keyed by a value an author has written into `$ui` (Ephemeral UI state) from a `list.display` row-press event; this contract does not require this specific pattern — `detail.source` MAY instead bind to a route- or context-supplied identifier — but Wire shapes and the automation-builder fixture illustrate it as the reference composition.

**[UIS-023]** `list.source` (UIS-020) MAY alternatively be a **paginated source**: an object `{path, paginated: true, limit?}` — `path` a Binding (UIS-100) naming the same collection a bare-string `list.source` would name, `paginated` present and literally `true`, `limit?` an optional integer overriding the host's default page size (`api/1` API-031's `[1, 200]` bound applies identically where supplied). A paginated `list.source` is fetched page-by-page using `api/1`'s keyset-pagination convention (`api/1` API-030–036: the `cursor`/`limit` request shape and the opaque `cursor` grammar API-036 defines — referenced here by name, not restated); `list.display` (UIS-020) renders against the accumulated, already-fetched rows exactly as it does for a plain-array `list.source`, growing as later pages arrive. `path` is Binding-typed, so a paginated source with a malformed `path` MUST fail validation as `BINDING_PATH_INVALID`, the same as any other Binding-typed field (UIS-066); a paginated source missing `path`, or whose `paginated` is not literally `true`, MUST fail the same way. A bare-string `list.source` (UIS-020's original form) remains valid and is unaffected by this option — it names an already-fully-resolved in-memory array (typically a small collection or a context feed, UIS-151) with no pagination of any kind; an author chooses the paginated form specifically for a collection too large to load in one page. A paginated source is a distinct, mutually exclusive alternate form from a LiveBinding (UIS-109) — this version does not define a combined live-and-paginated `list.source`.

**[UIS-024]** A `list-detail` page whose `list.source` is a paginated source (UIS-023) is rendered by the host with its own "load more" / infinite-scroll affordance — the same host-rendered-chrome pattern `newAction` (UIS-021) already establishes for the page's "new" affordance — that requests the next page using the most recently returned `cursor` and appends its `items` to the in-memory array `list.display` renders from; a page whose most recent fetch returned `cursor: null` (`api/1` API-032) MUST suppress that affordance, there being no further page to fetch. This contract does not define an author-authored ActionRef verb for pagination — ordinary `list.display` binding and rendering rules (UIS-020, UIS-071) apply unchanged to whatever rows have been fetched so far, exactly as they do for a plain-array `list.source`.

### Page types: settings-form

**[UIS-030]** A `settings-form` page MUST declare `source` as a root Binding (UIS-005) to the single record it edits, whose resolved value is the page's outermost Scope, and `sections` as a non-empty array of `{titleMsg?, fields}`, `fields` a non-empty array of widget nodes.

**[UIS-031]** A `settings-form` page MUST declare `actions` as a non-empty array of widget nodes (conventionally `button`, Widget catalog) rendered outside `sections`, at least one of which MUST wire `on.press` to a `submit` ActionRef (UIS-161).

### Page types: dashboard

**[UIS-040]** A `dashboard` page MUST declare `tiles` as an array of `{size, widget}`, `size` one of `small`, `medium`, `large` — the same closed three-value enum `manifest/1` MAN-061 uses for a fragment card's `sizeHint`, reused here for the same "how much of a grid cell" concept — and `widget` a widget node. A dashboard has no single page-wide bound resource (unlike `list-detail`/`settings-form`/`wizard`, UIS-005): each tile's `widget` and its own descendants resolve every Binding-typed field as a root Binding (UIS-005) independently, tile by tile, rather than against any page-wide Scope.

**[UIS-041]** A `dashboard` tile's `widget` SHOULD be a read-oriented widget (`stat-tile`, `table`, `text`, `badge`; Widget catalog) — this contract does not forbid an input-category widget inside a tile, but a dashboard page carries no `submit`-worthy bound resource of its own for such a widget's edits to land in.

### Page types: wizard

**[UIS-050]** A `wizard` page MUST declare `steps` as a non-empty array of `{id, titleMsg, root, canAdvanceIf?}`: `id` a Kebab identifier unique within the page, `titleMsg` a `msg:` reference, `root` a widget node, `canAdvanceIf` an optional BindingExpr gating the built-in `wizard-next` verb (Actions, UIS-160) — when present and falsy, `wizard-next` MUST be a no-op. A `wizard` page MUST declare `onFinish` as an ActionRef, invoked by the built-in `wizard-finish` verb (Actions, UIS-160).

**[UIS-051]** A `wizard` page MAY declare `draftSource` as a Binding naming a real backing resource the wizard progressively edits; when absent, every step's Scope is rooted at the reserved ephemeral path `$ui.draft` (Ephemeral UI state) instead, and `onFinish` is responsible for reading that ephemeral state and persisting it (typically via a `submit` or `call-action` ActionRef, Actions).

**[UIS-052]** Each step's `root` establishes its Scope from the wizard's draft root (UIS-051) — one shared Scope across all steps, not a per-step-isolated one — so a later step's bindings can read a value an earlier step wrote.

### Widget node envelope

**[UIS-060]** A widget node MUST declare `type` as a member of the closed Widget catalog (below); a `type` outside that set MUST fail validation as `UNKNOWN_WIDGET_TYPE` (Error taxonomy) — never silently rendered as an unstyled container or skipped. Growth of the widget catalog is exclusively a ui-schema/1 minor-version change (Negotiation), the same discipline RUL-001 applies to `rules/1`'s vocabulary.

**[UIS-061]** A widget node MAY declare `id` (a Kebab identifier, unique within its page document) for diagnostic and event-targeting purposes; this contract does not require `id` for rendering or binding to function.

**[UIS-062]** A widget node's `props` object MUST contain only keys its `type`'s Widget catalog entry declares; an unrecognized key MUST fail validation as `WIDGET_PROP_UNKNOWN`. A `props` key that entry marks required and that is absent MUST fail validation as `WIDGET_REQUIRED_FIELD_MISSING`. The same two rules apply identically to a widget node's `on` object against that entry's own closed event set, with `WIDGET_EVENT_UNKNOWN` in place of `WIDGET_PROP_UNKNOWN`.

**[UIS-063]** A widget node MAY declare `visibleIf` as a BindingExpr; the node (and, for a `children`-bearing type, its whole subtree) MUST NOT render while `visibleIf` evaluates falsy. `visibleIf`'s own absence is equivalent to a literal `true`. `visibleIf` gates rendering only — a hidden input-category widget's last-written bound value is untouched (the underlying data is not cleared merely because its field became hidden).

**[UIS-064]** A widget node MAY declare `children` as an array of widget nodes only when its `type`'s catalog entry marks the type children-bearing (Widget catalog); a `children`-bearing type without `children` renders as an empty container; a non-children-bearing type declaring `children` MUST fail validation as `WIDGET_PROP_UNKNOWN` naming `children` as the offending key.

**[UIS-065]** For an **input**-category widget (Widget catalog), `bind` MUST be present and is read-write by contract: the renderer both displays the Scope-resolved current value and writes the user's edits back to that same path, with no author-declared action required for the write itself. For a **display**-category widget, any Binding- or Computed-typed prop (e.g. `text`'s `value`) is read-only. `on.change`, where a widget's catalog entry lists it, is an optional additional hook fired alongside (never instead of) the intrinsic write.

**[UIS-066]** A widget node's `bind` (or an OptionSource's `data.source`, or any other Binding-typed field this contract defines) MUST be a syntactically valid Binding (Binding grammar: data paths); one that is not MUST fail validation as `BINDING_PATH_INVALID`. Where the widget's declared bind-shape (Widget catalog) and the Scope-resolved field's own declared type are both statically known to the validator, a mismatch MUST fail as `BINDING_TYPE_MISMATCH`; where the underlying field's type is not statically known (e.g. an untyped pack data-model field), this contract does not require static rejection — the mismatch is a rendering-time concern outside this contract's validation story.

### Widget catalog

**[UIS-070]** The closed widget catalog is exactly:

| type | category | children? | props (closed) | `bind` shape | events |
|---|---|---|---|---|---|
| `section` | structural | yes | `titleMsg?`, `collapsible?` (bool, default `false`), `defaultCollapsed?` (bool, default `false`) | none | none |
| `repeat` | structural | no | `itemTemplate` (widget node, required), `itemScope?` (Kebab identifier, default `item`), `minItems?` (int ≥0), `maxItems?` (int ≥ `minItems`), `emptyMsg?` (msg ref) | array | none |
| `switch` | structural | no | `discriminant` (BindingExpr, required), `cases` (non-empty array of `{when, render}`, `when` a JSON literal, `render` a widget node), `default?` (widget node) | none | none |
| `fragment` | structural | no | `ref` (Kebab identifier, required — a key of this document's `fragments`), `params?` (object of literal/Binding values) | Binding, optional — rescopes (UIS-183) rather than reading a value; same top-level field `repeat` uses for its array | none |
| `slot` | structural | no | `name` (Kebab identifier, required) | none | none |
| `text` | display | no | `value` (BindingExpr, required), `announce?` (`polite`\|`assertive`, UIS-077), `titleMsg?` (msg ref, UIS-079) | none | none |
| `badge` | display | no | `value` (BindingExpr, required), `tone?` (`neutral`\|`positive`\|`warning`\|`critical`, default `neutral`), `toneFrom?` (BindingExpr resolving to that same enum, UIS-078), `titleMsg?` (msg ref, UIS-079) | none | none |
| `table` | display | no | `source` (Binding, required, array), `emptyMsg?` / `emptyWidget?` (mutually exclusive), `loadingIf?` (BindingExpr), `searchPlaceholderMsg?` (UIS-071c), `columns` (non-empty array of `{headerMsg, cell}` or `{headerMsg, cellWidget, cellValue?}` — `cell` a BindingExpr and `cellWidget` a widget node, each evaluated per row under an implicit `item` scope, UIS-071/071a; each column MAY add `searchable?`, `filter?`, `filterLabelMsg?`, `numeric?`, UIS-071b) | none | `rowPress?` (ActionRef, `item` in scope) |
| `stat-tile` | display | no | `labelMsg` (required), `value` (BindingExpr, required), `tone?` (same enum as `badge`), `titleMsg?` (msg ref, UIS-079) | none | none |
| `text-input` | input | no | `labelMsg` (required, UIS-075), `placeholderMsg?`, `multiline?` (bool, default `false`), `maxLength?` (int) | string | `change?` |
| `number-input` | input | no | `labelMsg` (required, UIS-075), `min?`, `max?`, `step?` (number, default `1`) | number (nullable iff the bound field is declared nullable, UIS-072) | `change?` |
| `duration-input` | input | no | `labelMsg` (required, UIS-075), `displayUnit?` (`ms`\|`s`\|`min`\|`h`, default `s`), `min?` (number, seconds) | non-negative integer seconds | `change?` |
| `toggle` | input | no | `labelMsg` (required, UIS-075), `onLabelMsg?`, `offLabelMsg?` | boolean | `change?` |
| `select` | input | no | `labelMsg` (required, UIS-075), `options` (OptionSource, required), `placeholderMsg?` | scalar (string\|number\|boolean, matching `options`) | `change?` |
| `multi-select` | input | no | `labelMsg` (required, UIS-075), `options` (OptionSource, required) | array of scalar | `change?` |
| `entity-picker` | input | no | `labelMsg` (required, UIS-075), `bindShape?` (`entityRef`\|`entityId`, default `entityRef`, UIS-073a), `modes?` (non-empty subset of `["entity","selector","deviceClass"]`, default all three) | object — `rules/1` EntityRef shape (UIS-073) — or, when `bindShape` is `entityId`, a scalar entity-id string (UIS-073a) | `change?` |
| `time-of-day` | input | no | `labelMsg` (required, UIS-075) | string `HH:MM:SS` | `change?` |
| `button` | action | no | `labelMsg` (required), `style?` (`primary`\|`secondary`\|`destructive`, default `secondary`), `disabledIf?` (BindingExpr, UIS-076) | none | `press` (ActionRef, **required** — UIS-074) |

**[UIS-071]** `table`'s `columns[].cell` (and `rowPress`, when declared) evaluate under a per-row scope named `item`, established identically to `repeat`'s own item-scope rule (UIS-107) for each element of `source` in array order — `table` is, for binding purposes, `repeat` specialized to a tabular `itemTemplate`.

**[UIS-071a]** A `table` column MUST declare **exactly one** of `cell` (a BindingExpr, UIS-071) or `cellWidget` (a widget node). A `cellWidget` renders that subtree once per row under the very same `item` scope UIS-071 establishes for `cell`, so every construct this contract already defines — a `switch` over a three-state field, a `badge` whose tone comes from the row (UIS-078), a `button` whose `press` carries `item` (UIS-074) — composes inside a cell with no new mechanism. A column declaring neither MUST fail validation as `WIDGET_REQUIRED_FIELD_MISSING` naming `cell`; one declaring both MUST fail as `WIDGET_PROP_UNKNOWN` naming `cellWidget`, the two keys selecting mutually exclusive column shapes so that in a `cell` column `cellWidget` is a key its shape does not declare (UIS-062).

A `cellWidget` column MAY additionally declare `cellValue`, a BindingExpr supplying the single scalar that column contributes for **sorting and search**; a `cell` column MUST NOT declare it (`WIDGET_PROP_UNKNOWN`), its own BindingExpr already being that scalar. `cellValue` exists because a rendered subtree has no value a host can order or match against: without it a widget column is one an operator can neither sort by nor find a row through, and the sortability rule is otherwise unchanged — a `cellValue` naming a value on the row (a Binding or LiveBinding) sorts, a Computed or literal one does not, exactly as UIS-071 already provides for `cell`.

This is the construct that makes a **decision column** expressible. A tabular inventory whose rows each carry an inline choice — adopt or ignore a discovered device, retire a stale row — was not authorable in this catalog at all before it: `cell` renders a value and a value cannot be pressed, so a page needing per-row controls had no expression in `ui-schema/1` and had to leave the schema tier entirely. No `rowActions` slot is defined, and none is needed: a decision column is an ordinary column whose `cellWidget` is a `button`, which is why this requirement adds one column shape rather than a second per-row mechanism beside it. `cellWidget` and `cellValue` are OPTIONAL keys of a shape whose existing `cell` form is untouched, so their addition is a ui-schema/1 minor (Negotiation).

**[UIS-071b]** A `table` column MAY declare how the host's list affordances treat it: `searchable` (boolean) to narrow the search box's match set to the declaring columns, `filter` naming a member of the closed set `["enum"]` to give that column its own filter control, `filterLabelMsg` (a `msg:` reference) to label that control, and `numeric` (boolean) to align and order it as a figure. All four are optional and their absence preserves UIS-070's existing rendering exactly, so their addition is a ui-schema/1 minor (Negotiation).

`searchable` is opt-in **narrowing**, not opt-in enabling: when NO column declares it every column carrying a value is searched, so a page that declares none is unchanged, and a page that declares some is saying which of them the operator means. An `enum` filter's options are FACETED from that column's own distinct values, which is why no options declaration accompanies it — an author cannot state an option set that drifts from the rows actually present, because the author does not state one at all.

`filter` MUST be enforced against its closed set rather than presence-checked: a value outside it MUST fail validation as `BINDING_TYPE_MISMATCH`, never resolved to a default or silently dropped at render time. This is the treatment UIS-073a gives `bindShape` and UIS-144 gives `currencyCode`, for the reason those share — an unrecognized value renders NO control, and a filter an author declared and cannot see on the page is indistinguishable from one the data happened to make empty. A `filterLabelMsg` on a column that declares no `filter` MUST fail as `WIDGET_PROP_UNKNOWN`: it labels a control that does not exist.

Whether the host shows filter CHROME at all is not a separate author decision. A page declaring at least one `filter` has asked for it; a page declaring none MUST NOT be given it. This keeps the paging rule's principle intact — a host decides presentation from what the document said and what the data is, never from a prop this contract does not define — while leaving the author in control of the thing only an author knows, which is which columns are worth narrowing a fleet by.

This requirement exists because the closed catalog could describe a table's CONTENT and nothing about finding a row in it. Discovery spec §7 asks the device inventory for "search + filters"; the built page needs three enum filters (device class, reporting relay, adopt/ignore decision) and search narrowed to five of eleven columns, and none of that was expressible — so a page authored under UIS-070 alone was a strictly worse page than the hand-written one it would replace, on a fleet of sixty-odd rows where filtering is how an operator finds anything.

**[UIS-071c]** A `table` MAY declare what it shows when it has no rows, and when it is not yet in a position to say so: `emptyMsg` (a `msg:` reference) OR `emptyWidget` (a widget node) — never both — plus `loadingIf` (a BindingExpr) and `searchPlaceholderMsg` (a `msg:` reference). All are optional and their absence preserves UIS-070's rendering exactly, so their addition is a ui-schema/1 minor (Negotiation). A table declaring both empty forms MUST fail validation as `WIDGET_PROP_UNKNOWN` naming `emptyWidget`: an author who wrote both has two different empty states in mind, and silently applying one would leave the other rendered nowhere while still looking authored.

Two spellings exist because the two cases are genuinely different sizes. A list whose emptiness means one thing wants one sentence. A list whose emptiness has SEVERAL CAUSES has to say which, and that is a `switch` over a bound discriminant — an ordinary widget subtree needing no new grammar. This contract does not otherwise let a host decide an empty state's content, and it must not: an empty list is the single most misread state on any operator surface, because the reading an operator reaches for — *"there is nothing out there"* — is exactly the one that is wrong when the truth is *"nothing has been able to look"*.

`loadingIf` exists because that misreading has a second, worse moment. A table that has not fetched yet and a table that fetched nothing are IDENTICAL in the data, so a page without this paints its empty state during the first fetch and states that the network is empty while it is still being read. While `loadingIf` evaluates truthy the host MUST show a loading affordance and MUST NOT show the empty state; its absence is equivalent to a literal `false`, which preserves every existing page's behaviour.

`searchPlaceholderMsg` labels what the search box actually matches. It is not decoration: UIS-071b lets a document narrow the searched set to some columns, and a search box whose scope is invisible is one an operator cannot form a query for.

**[UIS-072]** A `number-input` bound to a field whose declared type admits `null` (e.g. a mode's `max`, `rules/1` Wire shapes) MUST write `null` when the user clears the field, never `0` or a non-numeric placeholder value.

**[UIS-073]** An `entity-picker`'s bound value MUST be an object matching exactly one of the three forms `rules/1`'s EntityRef defines (RUL-010): `{entity_id}`, `{selector}`, `{device_class}`. A `selector` value, when present, MUST be a syntactically valid Selector string in `api/1`'s label-selector grammar (API-040–046) — this contract does not restate that grammar, only requires conformance to it (API-046's own "any field elsewhere that accepts a label selector... MUST accept exactly this grammar" already states the obligation from the other side). A `device_class` value's candidate list is the device-class registry's own `device_classes` keys (REG-002), resolved by the host; this contract's binding grammar does not enumerate them. `modes` restricts which of the three forms the rendered picker offers; it does not change the bound value's own shape rule. This requirement states the **`entityRef`** binding shape's own value rule — the shape a picker uses by default; UIS-073a defines the alternative scalar shape and how a node declares which of the two it uses.

**[UIS-073a]** An `entity-picker` MAY declare `bindShape` — `entityRef` (the default) or `entityId` — naming which of two **binding shapes** its own `bind` addresses. Under `entityRef` the `bind` names an EntityRef **object** and the picker reads and replaces that whole object, exactly as UIS-073 requires. Under `entityId` the `bind` names a single **scalar entity-id string** directly: the shape `rules/1` itself writes, since RUL-010 inlines `entity_id` into the trigger/condition/action object rather than nesting an EntityRef sub-object there, leaving a rules/1 record with no EntityRef-shaped sub-object for an `entityRef` picker to bind to. Under `entityId` an edit MUST write the bound scalar path itself and MUST NOT write its parent object: every sibling member of the record the scalar sits in (a `state` trigger's own `type`, `to`, `for`, …) survives the write untouched. A renderer that instead replaced that parent with a freshly-built `{entity_id: …}` — the write an `entityRef` picker performs — would erase those siblings and is non-conformant, which is precisely why the scalar shape exists rather than binding an `entityRef` picker to the enclosing object. The two shapes are never inferred from the bound value at render time: a node states which one it uses, or takes the default, so a reader tells them apart from the document alone. `bindShape` is enforced against that closed two-member set rather than merely presence-checked — the same enforced-not-merely-skipped treatment UIS-144 gives `formatCurrency`'s `currencyCode`, and for the same reason: a value outside `entityRef`/`entityId` MUST fail validation as `BINDING_TYPE_MISMATCH`, never resolved to the default at render time, because a typo there silently reverts the picker to the object shape and paints an empty control over a record that does carry an entity — the exact failure the scalar shape exists to remove. Because a scalar carries no discriminant, only UIS-073's `entity` form is expressible under `entityId`: a node declaring `bindShape: "entityId"` MUST leave `modes` absent or declare it as exactly `["entity"]`, and one naming `selector` or `deviceClass` there MUST fail validation as `BINDING_TYPE_MISMATCH` (Error taxonomy) — a form the declared binding shape cannot carry is a static contradiction, never silently dropped at render time (UIS-200). `bindShape` is an OPTIONAL prop whose default preserves UIS-073's shape exactly, so its addition is a ui-schema/1 minor (Negotiation).

**[UIS-074]** A `button` node MUST declare `on.press`; one that does not MUST fail validation as `WIDGET_REQUIRED_FIELD_MISSING` naming `on.press`.

**[UIS-075]** Every **input**-category widget (Widget catalog) — `text-input`, `number-input`, `duration-input`, `toggle`, `select`, `multi-select`, `entity-picker`, `time-of-day` — MUST declare `labelMsg` as a required prop: a `msg:` reference (Definitions), the same reference-catalog convention `manifest/1` MAN-003/MAN-111 already establishes and this contract already requires of `button` (UIS-074) and `stat-tile`. A node of one of these eight types omitting `labelMsg` MUST fail validation as `WIDGET_REQUIRED_FIELD_MISSING` naming `labelMsg` (UIS-062) — packs ship no frontend code of their own (Scope), so `labelMsg` is the only place an accessible name for a form control can come from; there is no default derivation from a nearby `text`/`section` `titleMsg` or from the field's own data-path segment. `labelMsg` is that widget's own accessible label (the a11y-tree/screen-reader name for that one control) and is independent of every other label-shaped prop this contract defines on the same or a different widget: `placeholderMsg` (`text-input`, `select`) is placeholder text shown only in an empty control, never an accessible-name substitute; `onLabelMsg`/`offLabelMsg` (`toggle`) label the two state values, not the field itself; a `section`'s `titleMsg` labels a group, not any one control inside it. This contract does not carve out an exception for a widget whose label might seem derivable from surrounding layout (e.g. two adjacent `time-of-day` fields under one `section` titled "Active hours") — each input-category node still requires its own `labelMsg` (e.g. distinct "Start time"/"End time" references), never a shared reliance on the section's own heading.

**[UIS-076]** A `button` node MAY declare `disabledIf` as a BindingExpr (UIS-141). While it evaluates truthy the control MUST still render, MUST be un-pressable, and MUST be marked disabled to assistive technology; pressing it MUST NOT dispatch its `on.press` ActionRef (UIS-074), and MUST NOT open a `confirm` (UIS-165) either — a disabled control has no path to its verb at all, not merely a path that stops short. `disabledIf`'s absence is equivalent to a literal `false`.

`visibleIf` (UIS-063) is not a substitute for it and this contract does not treat the two as interchangeable. An absent control says *this action does not apply here*; a disabled control says *this action applies, but not right now* — and a page forced to express the second by removing the first would delete and re-insert its own primary control at the moment the operator reaches for it, moving the layout under the cursor and discarding whatever had focus. `disabledIf` is also the only construct in this contract that can make a widget-triggered invocation **non-re-entrant**: an ActionRef publishing an ActionOutcome (UIS-166) is in flight for exactly as long as that outcome reads `pending`, so `disabledIf` bound to `pending` is how a page stops a second press from dispatching a second `delete` or a second `call-action` while the first has not answered. Without it, closing the outcome gap would hand every author a state they can *display* and cannot *act on*.

`disabledIf` is defined on `button` alone. An **input**-category widget's own disabled/read-only state is a different question — whether a value may be edited, not whether an action may fire — and this version does not define it. `disabledIf` is an OPTIONAL prop whose absence preserves UIS-070's existing rendering exactly, so its addition is a ui-schema/1 minor (Negotiation).

**[UIS-077]** A `text` node MAY declare `announce` as one of `polite` or `assertive`; when present the rendered text MUST be an ARIA live region of that politeness, so text that appears — or changes — after the page has already been read is announced rather than silently painted. `announce`'s absence is the default: an ordinary, non-live text node, exactly as UIS-070 has always rendered one.

It exists because closing the outcome gap (UIS-166) without it would produce pages that are *worse* than the ones it replaces. An ActionOutcome's whole purpose is to appear after the operator has acted — a refusal sentence, a "working on it", a completion — and a `visibleIf`-gated `text` node that materializes into an already-read document is, to a screen-reader user, silence. Packs ship no frontend code of their own (Scope), so a live region is not something an author can add outside this catalog; the same reasoning UIS-075 gives for `labelMsg` being the only place an accessible name can come from applies here to the only place an announcement can come from.

A value outside the closed two-member set MUST NOT degrade to a non-live region: a host resolves an unrecognized politeness to `polite`. This contract defines no taxonomy code for a wrong scalar value and does not invent one here (the same presence-only treatment `tone`/`style`/`displayUnit` get), so the failure mode of a typo is chosen deliberately rather than left to a host — an over-announcement is recoverable, an announcement that never happens is exactly the defect the prop exists to prevent, and it is invisible to everyone who can see the screen. `announce` is an OPTIONAL prop, so its addition is a ui-schema/1 minor (Negotiation).

**[UIS-078]** A `badge` node MAY declare `toneFrom`, a BindingExpr (UIS-141) resolving to a member of the same closed tone vocabulary `tone` names. When `toneFrom` is present it DETERMINES the rendered tone and `tone` is ignored — a document declaring both has stated that the tone depends on the data, and preferring the static one would paint a single tone down every row of exactly the table the prop exists for.

`tone` itself is deliberately NOT widened to accept a BindingExpr. A bare string is a Binding **path** in that grammar (UIS-108), so widening would silently reinterpret every `tone: "warning"` already written as a lookup of a field named `warning`, resolve it to nothing, and repaint those badges with the fallback tone — a change that breaks documents without failing validation anywhere. The static prop and its dynamic sibling are separate keys for the same reason UIS-073a keeps `bindShape` declared rather than inferred: which of two readings applies is stated in the document, never guessed from the shape of a value.

A resolved value outside the closed vocabulary MUST render as `neutral`. A dynamic tone cannot be refused at page load — the value does not exist until the data does, so UIS-200's static-refusal rule has nothing to act on — which leaves only the question of which wrong answer to give, and `neutral` is the one that asserts nothing: degrading to `positive` would paint a failed device green, and rendering no badge at all would delete the row's status entirely. This is the same choice UIS-077 makes for an unrecognized `announce`, for the same stated reason. `toneFrom` is an OPTIONAL prop whose absence preserves UIS-070's rendering exactly, so its addition is a ui-schema/1 minor (Negotiation).

**[UIS-079]** A `text`, `badge` or `stat-tile` node MAY declare `titleMsg`, a `msg:` reference (Definitions) rendered as explanatory hover text on the value it labels. It is resolved through the same reference catalog every other operator-facing string uses (MAN-003/111), so an explanation stays translatable rather than becoming the one untranslated sentence on a page.

It exists because this catalog's display widgets could render a *state* and not the sentence that makes the state mean anything. A three-state cell distinguishing "nothing has scanned this device" from "a scan found nothing open" is only distinguishable to an operator who is told which is which — absent that, the two read as the same shrug, and the distinction the layers below fought to preserve is discarded at the last step. Packs ship no frontend code of their own (Scope), so as with `labelMsg` (UIS-075) and `announce` (UIS-077) there is no other place such a sentence can come from.

`titleMsg` is supplementary, never load-bearing: it MUST NOT be the only carrier of information required to operate the page, since hover text is unreachable by touch and inconsistently surfaced by assistive technology. A page needing the distinction to be *readable* states it in a `text` node; `titleMsg` explains what is already visible. It is an OPTIONAL prop whose absence preserves UIS-070's rendering exactly, so its addition is a ui-schema/1 minor (Negotiation).

### Binding grammar: data paths

**[UIS-100]** A Binding MUST match the grammar `path := segment ("." segment)*`, `segment := name | name "[" index "]" | name "[" predicate "]"`, `name := <a Kebab identifier, OR a reserved root token — $root, $ui, $context, $params, item — OR an ordinary field-name matching ^[A-Za-z_][A-Za-z0-9_]*$>`, `index := <non-negative integer literal> | "$index"`, `predicate := name "=" (<JSON literal> | path)`. This dotted, bracket-indexed style is the same notation `rules/1` RUL-006 already uses informally for diagnostic field paths (e.g. `actions[0].default[0]`), promoted here to a full normative grammar. A Binding not matching this grammar MUST fail validation as `BINDING_PATH_INVALID`.

**[UIS-101]** A `predicate` segment (`name "[" field "=" value "]"`) selects the first element of the array at `name` whose own `field` equals `value` (`value` disambiguated per UIS-108 — a number is a literal, a bare string is a Binding resolved before the comparison, `{"lit": ...}` forces a string literal); a predicate segment matching zero elements resolves the whole containing path to absent/null rather than raising; matching more than one element resolves to the first match in array order. An author needing a unique result predicates on a field that is unique across the array (e.g. an `id`).

**[UIS-102]** A field-name segment MUST NOT begin with `$` — that prefix is reserved exclusively for this contract's own root tokens (UIS-103–106); a widget catalog implementation MUST treat a real data field beginning with `$` as unreachable by this grammar (a pack author avoids such field names by construction, the same closure discipline `manifest/1`'s own reserved-prefix conventions rely on elsewhere).

**[UIS-103]** A Binding with no reserved-root prefix resolves relative to the innermost enclosing Scope (UIS-005, UIS-107, UIS-183). A Binding prefixed `$root.` resolves from the page's outermost Scope regardless of how deeply nested the widget node is — an explicit escape from whatever `repeat`/`fragment` rescoping is in effect.

**[UIS-104]** `$ui.<path>` addresses **ephemeral UI state**: page-scoped, renderer-held, read-write from any widget node on the page, initialized absent at page load, and never persisted, submitted, or included in any `submit`/`call-action` ActionRef's implicit payload (Actions) — a page uses it for pure interaction bookkeeping (which list-detail row is selected, an in-progress wizard draft when `draftSource` is absent, UIS-051) that has no bearing on the underlying resource. `$ui` state resets to absent on navigation away from the page.

**[UIS-105]** `$context.<name>` addresses a Context feed (Context feeds); `<name>` MUST be a key of the page document's own `context` map (UIS-004). A `$context.<name>` Binding whose `<name>` is not declared in that map MUST fail validation as `CONTEXT_REF_UNDEFINED`.

**[UIS-106]** `$params.<name>` addresses a parameter passed into the current `fragment` invocation (UIS-183); it is valid only inside a fragment's own widget-node subtree, resolved against that specific invocation's `params`.

**[UIS-107]** Inside a `repeat`'s `itemTemplate` (or a `table`'s per-row scope, UIS-071), the enclosing Scope narrows to the current element: the reserved segment named by `itemScope` (default `item`) addresses that element, `<itemScope>.$index` addresses its zero-based position (read-only — a Binding MUST NOT target `$index` as a write destination; doing so MUST fail validation as `BINDING_PATH_INVALID`), and an unprefixed segment resolves against that same element. A nested `repeat` **shadows** the outer `itemScope` name within its own `itemTemplate` (the inner `item` wins); this contract provides no ancestor-scope or parent-array Binding token by design — reaching the page root uses `$root.` (UIS-103), and removing an element from the very array a template iterates uses `repeat-remove`'s item self-reference (UIS-162), never a path walk back up to the containing array (which the shadow would in any case make unaddressable).

**[UIS-108]** Wherever this contract types a value position as **literal or Binding** — a Computed's generic `args` entry (Binding grammar: computed values), except where that function's own signature pins an argument to a narrower type (`label`'s `vocabRef`, `firstKey`'s literal key-array, the `arrayBinding`/`secondsBinding`/`valueBinding` arguments); an ActionRef field documented as `literal or Binding` or `object of literal/Binding` (Actions); and a predicate segment's `value` (UIS-101) — the value's JSON type disambiguates it: a JSON **number**, **boolean**, or **null** is that literal value; a JSON **string** is a **Binding** (data-path grammar, UIS-100), never a bare string literal; a JSON **object** carrying a `compute` key is a Computed, one carrying a `lit` key is an explicit literal whose value is `lit`'s own value verbatim (`{"lit": "parallel"}` is the string literal `"parallel"` — the sole escape hatch for a string literal in a Binding-or-literal position, and `compute` is checked before `lit` so the two never contend); any other object, or a JSON array, is a literal as-is. A position this contract types as **literal only** (an OptionSource `literal` item's `value`, UIS-131; a `repeat-add`/`create` `itemDefault`, Actions) never applies the string-is-Binding rule — its strings are always literals.

**[UIS-109]** A field this contract types as a Binding — via UIS-066 directly (`bind`, a root Binding UIS-005, an OptionSource's `data.source`, or any other field UIS-066 names), or via the Binding branch of a BindingExpr-or-literal-or-Binding position's UIS-108 disambiguation (a bare JSON string) — MAY instead be a **LiveBinding**: an object `{path, live: true}`, `path` itself a syntactically valid Binding (UIS-100), `live` present and literally `true`. A LiveBinding's `path` key makes it unambiguous against every other object shape those same positions accept — a Computed's `{compute, args}`, an OptionSource's `{kind, ...}`, UIS-108's `{lit: ...}` literal escape — none of which carries `path`. This alternate form does not extend to a position UIS-108 already excludes from the string-is-Binding rule (a **literal-only** position, e.g. an OptionSource `literal` item's `value`, a `repeat-add`/`create` `itemDefault`) — a `{path, live}`-shaped object appearing there is an ordinary object literal, not a LiveBinding. Omitting `live`, or a bare Binding string (UIS-100), is the ordinary once-fetched form (UIS-110); `{path, live: false}` is not a form this contract defines — a once-fetched Binding is always written as the bare string. A LiveBinding missing `path`, or whose `live` is not literally `true`, MUST fail validation as `BINDING_PATH_INVALID` — the same code an ordinary malformed Binding string already fails as (UIS-100).

**[UIS-110]** A LiveBinding (UIS-109) re-evaluates for as long as its widget node stays mounted; updates MUST be delivered to the renderer over `events/1` — reusing that contract's WS/SSE transport, authentication, and scope-node filtering exactly as `events/1` itself defines them (Scope), never redefined here. The mapping from a specific LiveBinding to the `events/1` delivery(s) that trigger its re-evaluation is a rendering-engine concern this contract does not specify, the same "rendering engine's own implementation... a host concern" carve-out this contract's Scope section already states generally. A bare-string Binding (UIS-100) is instead fetched exactly once, at the point its enclosing Scope is established (page load; a `repeat` item's creation, UIS-107; a `fragment` invocation's own scope resolution, UIS-183) and MUST NOT silently re-fetch on its own — an author opts into freshness beyond that one read explicitly, via `live: true`. For an **input**-category widget's `bind` (UIS-065), a live re-evaluation updates the displayed value the same as a fresh read would; this contract does not define how a renderer reconciles that update against the user's own in-progress, not-yet-written edit in the same field at the moment a live update arrives — also a rendering-engine concern.

### Binding grammar: vocabulary references

**[UIS-120]** A vocabulary reference (vocabRef) is a string of the form `<namespace>:<name>` — the same `<prefix>:<name>` shape `manifest/1`'s own `msg:` convention already establishes (MAN-003), applied here to a different closed namespace. The complete closed set of vocabRef values is exactly:

| vocabRef | source | members |
|---|---|---|
| `rules/1:trigger-kind` | `rules/1` Triggers | `state`, `numeric`, `time`, `time_pattern`, `sun`, `template`, `event`, `webhook` |
| `rules/1:condition-kind` | `rules/1` Conditions | `and`, `or`, `not`, `state`, `numeric`, `time`, `sun`, `variable`, `template` |
| `rules/1:condition-leaf-kind` | `rules/1` Conditions (RUL-100 leaf subset) | `state`, `numeric`, `time`, `sun`, `variable`, `template` |
| `rules/1:action-kind` | `rules/1` Actions | `device_command`, `preset_batch`, `choose`, `delay`, `log`, `notify`, `variable_write`, `workflow_start`, `pack_action` |
| `rules/1:mode` | `rules/1` Modes | `single`, `restart`, `queued`, `parallel` |
| `rules/1:misfire` | `rules/1` Misfire policy | `catch_up_once`, `skip`, `fire_each` |
| `rules/1:filter` | `rules/1` Expression grammar and filters | `state`, `attr`, `default`, `upper`, `lower`, `trim`, `round`, `abs`, `int`, `float`, `now`, `elapsed`, `duration`, `convert` |

**[UIS-121]** A vocabRef used where this contract requires one (an `options` prop's `vocab`-kind OptionSource, Binding grammar: option sources) and not present in the UIS-120 table MUST fail validation as `VOCAB_REF_UNKNOWN`. Growth of this table tracks `rules/1`'s own vocabulary growth: a `rules/1` minor that adds a trigger/condition/action/mode/misfire/filter member is additive here too (this contract's own Negotiation still governs when a ui-schema/1 document may rely on the addition).

**[UIS-122]** A vocabRef names a closed set exhaustively — it is never partially restated. A picker bound to `rules/1:trigger-kind`'s OptionSource, for instance, has all eight members available regardless of whether every member is meaningful in a given page; a page MAY further restrict which members its own `switch` (Widget catalog, UIS-070) provides cases for without that restriction touching the OptionSource's own completeness. A page needing a picker over only *part* of an existing vocabRef's closed set — e.g. a leaf-only condition-kind picker that must never offer the composition keys `and`/`or`/`not` as a leaf's `type` value, since `rules/1`'s Condition shape (RUL-100) discriminates composition from leaf by key presence, not by `type`'s value at all — does so via a **separate, dedicated vocabRef row** scoped to exactly that subset (e.g. `rules/1:condition-leaf-kind`, UIS-120), never by omitting members from the wider vocabRef's own `labels` map (UIS-132 already forbids that) and never by an OptionSource-level subset/filter mechanism, which this contract does not define.

**[UIS-123]** This contract's Computed grammar (Binding grammar: computed values) is deliberately separate from `rules/1`'s Expression grammar (RUL-280–292): different sources (already-bound page data, never a live entity/attribute lookup), a different closed function list, and a different purpose (view computation for display, never automation evaluation). Where a page needs to present a value that is itself a `rules/1` Expression (e.g. a `log` action's `message`, or a `template` trigger's `expression`), it does so as opaque string data through `text-input`/`text` — this contract's binding grammar never parses or evaluates a `rules/1` Expression pipeline.

### Binding grammar: option sources

**[UIS-130]** A `select`/`multi-select` widget's `options` prop MUST be an OptionSource: an object `{kind, ...}` where `kind` is exactly one of `literal`, `vocab`, `data`. A `kind` outside this set, or a `kind`-specific required field's absence, MUST fail validation as `OPTION_SOURCE_INVALID`.

**[UIS-131]** `{kind: "literal", items}` — `items` a non-empty array of `{value, labelMsg}`, `value` a JSON scalar, `labelMsg` a `msg:` reference. The candidate set is exactly `items`, in the declared order.

**[UIS-132]** `{kind: "vocab", ref, labels}` — `ref` a vocabRef (Binding grammar: vocabulary references); `labels` an object mapping every member of `ref`'s closed set to a `msg:` reference. A `labels` map omitting any member of `ref`'s set MUST fail validation as `OPTION_SOURCE_INVALID`. The candidate set is `ref`'s members, each paired with its `labels` entry, in the order UIS-120's table lists them.

**[UIS-133]** `{kind: "data", source, valuePath, labelPath}` — `source` a Binding to an array (typically `$context.<name>`, Context feeds, or a page-local collection path); `valuePath`/`labelPath` field names read from each element of that array (relative to the element, using the same grammar as an `itemScope`-relative path, UIS-107) to produce that candidate's value and display label respectively. The candidate set is computed at render/edit time from `source`'s current contents — it is not a closed, contract-fixed set the way `literal` and `vocab` are.

**[UIS-134]** A `select`/`multi-select` bound value that does not match any candidate in its own current OptionSource is a rendering-time concern (e.g. a stale reference into a `data`-kind source whose row was since removed) — this contract does not require validation-time rejection of a bound value against a `data`-kind OptionSource's necessarily-dynamic contents, only against a `literal` or `vocab` source's closed set, where a bound literal outside the declared set MUST fail as `BINDING_TYPE_MISMATCH`.

### Binding grammar: computed values

**[UIS-140]** A Computed is `{compute, args}`, `compute` one of the closed function names below, `args` an array of Binding, JSON literal, or nested Computed values. A `compute` name outside this list MUST fail validation as `COMPUTE_FN_UNKNOWN`. A generic `args` entry — one the function's signature below does not pin to a narrower type (`label`'s `vocabRef`, `firstKey`'s literal key-array, `formatCurrency`'s pinned currency code, and the `arrayBinding`/`secondsBinding`/`valueBinding`-typed arguments are the pinned ones) — is disambiguated between a literal and a Binding per UIS-108: notably, a bare string `arg` is a **Binding**, so a string literal argument (as in `eq(mode, "parallel")`) MUST be written `{"lit": "parallel"}`.

| compute | signature | behavior |
|---|---|---|
| `eq` | `eq(a, b) -> boolean` | `a` equals `b` (typed equality, no coercion) |
| `not` | `not(a) -> boolean` | boolean negation |
| `and` | `and(a, b, ...) -> boolean` | true iff every argument is truthy |
| `or` | `or(a, b, ...) -> boolean` | true iff any argument is truthy |
| `count` | `count(arrayBinding) -> number` | length of the bound array (0 if absent) |
| `isEmpty` | `isEmpty(arrayBinding) -> boolean` | true iff the bound array is absent or has length 0 |
| `join` | `join(arrayBinding, sep) -> string` | array elements joined with `sep` |
| `label` | `label(vocabRef, valueBinding) -> string` | resolves `valueBinding`'s current value against `vocabRef`'s `vocab`-kind label map in whatever `select`/`multi-select` on the same page declares one for that `vocabRef`; a page with no such declaration for the referenced `vocabRef` MUST declare one via a `{kind:"vocab", ref, labels}` OptionSource somewhere in the same document for `label` to resolve — this contract does not carry a second, parallel label registry |
| `msg` | `msg(msgRef, ...argBindings) -> string` | resolves `msgRef` (`manifest/1` MAN-003/111) with `argBindings`' current values interpolated positionally |
| `formatDuration` | `formatDuration(secondsBinding) -> string` | human-readable rendering of a whole-seconds value |
| `formatDate` | `formatDate(dateBinding) -> string` | locale-formatted rendering of an ISO-8601 date or date-time string value, per the viewer's active locale (UIS-143) |
| `formatNumber` | `formatNumber(numberBinding) -> string` | locale-formatted rendering of a numeric value (grouping/decimal separators), per the viewer's active locale (UIS-143) |
| `formatCurrency` | `formatCurrency(numberBinding, currencyCode) -> string` | locale-formatted currency rendering of a numeric value (symbol, grouping/decimal separators, symbol placement) per the viewer's active locale, pinned to the ISO 4217 `currencyCode` (a literal, not a Binding — the priced value's currency is a business fact, not a viewer preference) (UIS-143) |
| `firstKey` | `firstKey(objectBinding, candidateKeys) -> string \| null` | the first of `candidateKeys` (a literal array of strings) that exists as a key on the object at `objectBinding`, or `null` if none exist |

**[UIS-141]** `visibleIf`, a wizard step's `canAdvanceIf`, and any other BindingExpr-typed field this contract defines accept a bare Binding (truthy/falsy on its resolved value), a Computed, or a JSON literal boolean — all three are valid BindingExpr forms (Definitions).

**[UIS-142]** `firstKey`'s purpose is expressing a discriminated union keyed by **which field is present**, distinct from `switch`'s ordinary case of matching a single field's **value** — `rules/1`'s own Condition shape (RUL-100) is exactly this: a composition (`and`/`or`/`not`) and a leaf (`type`) are told apart by which top-level key exists on the object, not by a shared field's value, since a composition carries no `type` key at all. A `switch` node's `discriminant` (UIS-070) accepting a BindingExpr rather than only a bare Binding is what makes this composable: `{"compute": "firstKey", "args": [<binding to the object>, ["and", "or", "not", "type"]]}` normalizes such a shape into the single string value an ordinary `case`/`when` match consumes, with no separate "key-presence" case-matching mode needed in `switch` itself.

**[UIS-143]** `formatDate`, `formatNumber`, and `formatCurrency` (Binding grammar: computed values) render their bound value using the same viewer-resolved locale a `msg:` reference resolves its locale catalog against (`manifest/1` MAN-111) — this contract defines no separate locale-resolution mechanism of its own, the same "reused here by reference" treatment its Scope section already gives the `msg:` mechanism generally. A page's date-, number-, or price-typed field is never implicitly formatted merely by virtue of its type: `text`/`badge`/`stat-tile`'s `value` (or a `table` column's `cell`) reads a raw Binding verbatim unless the author explicitly wraps it in `formatDate`/`formatNumber`/`formatCurrency` — the same already-optional, author-invoked usage `formatDuration` has always had — so a page needing a raw, non-localized value (an ID, a code, a wire-format timestamp meant for copy-paste, or an editable `number-input`'s own bound value) continues to bind directly. `formatCurrency` additionally pins WHICH currency the value is (its `currencyCode` argument, an ISO 4217 code) independently of the viewer's locale — the locale governs only the rendering's symbol placement and separators, exactly as `Intl.NumberFormat`'s own `{style: "currency", currency}` option separates the two; a price does not change currency because a different viewer is looking at it. This is what it means for date/number/currency **display** values to be locale-formatted rather than raw: the contract supplies the formatting vocabulary a pack needs since it ships no frontend code of its own to call `Intl` or an equivalent itself (Scope); which value positions are worth formatting remains an authoring choice.

**[UIS-144]** `formatCurrency`'s `currencyCode` argument (UIS-140, UIS-143) MUST be present and a well-formed ISO 4217 code (three alphabetic characters, matched case-insensitively) — the same enforced-not-merely-skipped treatment `label`'s `vocabRef` argument gets (UIS-121): the validator does not merely decline to grammar-check it as a Binding, it separately checks its presence and shape. An absent (args entry missing or `args` too short), non-string, or malformed `currencyCode` MUST fail validation as `CURRENCY_CODE_INVALID`, never rendered — a `currencyCode` typo or omission is a business-fact error in the priced row (which currency), not a cosmetic one, so it MUST NOT reach render time and silently resolve to some fallback currency there.

### Context feeds

**[UIS-150]** A page document's `context` map (UIS-004) entries are ContextRef objects: `{collection}`, `collection` a string naming either one of the pack's own `manifest/1` `dataModel.collections[].name` entries (MAN-051) or a platform-owned collection name — this contract does not enumerate platform-owned collection names, the same "referenced by name only" treatment `rules/1`'s own Scope section gives the preset-batch row it doesn't own (RUL-170's scope note).

**[UIS-151]** `$context.<name>` (UIS-105) resolves, at render time, to `collection`'s current row set as an array — the same shape a `data`-kind OptionSource's `source` or a `table`/`repeat`'s own array `bind` consumes; a context feed is not a distinct data type, only a distinctly-named entry point into one.

**[UIS-152]** A context feed is read-only: no Binding grammar construct in this contract writes through `$context.*`. A page needing to mutate a context feed's underlying collection does so via an ActionRef (`call-action` or `submit`, Actions) naming that collection's own write path, never via a `$context`-prefixed `bind`.

### Actions

**[UIS-160]** A widget node's `on.<event>` value MUST be an ActionRef: `{verb, ...fields}`, `verb` one of the closed set below. A `verb` outside this set MUST fail validation as `ACTION_VERB_UNKNOWN`; a `verb`-specific required field's absence, or a field of the wrong shape, MUST fail as `ACTION_FIELDS_INVALID`. Two further fields are **envelope** fields rather than any one verb's own — both OPTIONAL, both governing HOW the named verb is invoked rather than what it does: `confirm` (UIS-165) and `outcomeTo` (UIS-166).

| verb | fields | effect |
|---|---|---|
| `navigate` | `to` (path template string), `params?` (object of literal/Binding) | Navigates to another page path, substituting `params` into `to`'s placeholders. |
| `submit` | `target?` (Binding, default the page's own primary bound resource) | Persists the bound resource via the host's ordinary write path for that resource. This contract does not name the specific management-API route (`api/1`'s concern). |
| `create` | `target` (Binding to a collection), `itemDefault?` (literal object, default `{}`), `scopeFrom?` (Binding), `lifecycle?` (literal string) | Creates a new record in the named collection via the host's ordinary create path, seeded from `itemDefault` and, for the `list-detail` create idiom (UIS-021), the universal-envelope defaults `scopeFrom` (→ the new row's `scope_node`) and `lifecycle` (→ its `lifecycle_state`) supply (UIS-164). |
| `delete` | `target` (Binding, required) | Deletes/archives the bound resource via the host's ordinary delete/lifecycle path. |
| `call-action` | `action` (a `manifest/1` `actions[].name`, required), `params?` (object of literal/Binding) | Invokes a pack-declared action (MAN-100/101) with the given params, dispatched exactly as a management-API invocation of that action would be (`ctx/1` CTX-110), subject to MAN-104's `execution: relay-command` routing exception. |
| `set` | `target` (Binding, required), `value` (literal or Binding) | Writes `value` to `target`. The most common use is writing into `$ui` (UIS-104); writing into the page's own bound resource is also valid and is an ordinary local edit, distinct from persisting it (`submit`). |
| `repeat-add` | `target` (Binding to an array, required), `itemDefault?` (literal object, default `{}`) | Appends a deep clone of `itemDefault` to the array at `target`. |
| `repeat-remove` | `target` (an itemScope reference — the repeat item to remove, required; typically the innermost `item`) | Removes the referenced repeat item from the array its enclosing `repeat` iterates. The renderer resolves the array and the index from the item's own iteration context (which `repeat` instance rendered it, at which position), so neither an array path nor an index is supplied — this is the only way to remove an element from inside the template that iterates it, since the containing array is not addressable by path there (UIS-107). |
| `wizard-next` | none | Advances the enclosing `wizard` to its next step, subject to that step's `canAdvanceIf` (UIS-050); a no-op past the last step. Valid only inside a `wizard` page. |
| `wizard-back` | none | Returns the enclosing `wizard` to its previous step; a no-op before the first step. Valid only inside a `wizard` page. |
| `wizard-finish` | none | Invokes the enclosing `wizard`'s own `onFinish` ActionRef (UIS-050). Valid only inside a `wizard` page. |

**[UIS-161]** `submit`'s and `create`'s "ordinary write path" — and `delete`'s "ordinary delete/lifecycle path" — resolve identically to however that same resource is created, written, or removed through the management API; this contract fixes only that a widget-triggered persist/create/delete goes through that same path, never a second, UI-only write mechanism.

**[UIS-162]** `repeat-add`/`repeat-remove` operate on the array's in-memory bound value directly (the same array a `repeat` or `table` widget renders from); they take effect immediately and participate in whatever `submit` later persists the containing resource — they do not themselves persist anything. `repeat-add`'s `target` is an ordinary Binding to the array, resolved in the scope where the add affordance sits (conventionally a sibling of the `repeat`, outside its `itemTemplate`, where the array is directly addressable). `repeat-remove`'s `target` is instead an itemScope reference to the item being removed (UIS-160): an author places the remove affordance inside the `itemTemplate` and targets the innermost `item`, and the renderer removes exactly that rendered element. The array itself is never named — which is what lets removal work identically at every nesting depth, including inside a self-referential fragment where the containing array is unaddressable by path (UIS-107, UIS-182). A `repeat-remove` whose `target` is not an itemScope reference in scope at that node MUST fail validation as `ACTION_FIELDS_INVALID`.

**[UIS-163]** `wizard-next`/`wizard-back`/`wizard-finish` used outside a `wizard` page's own widget-node subtree MUST fail validation as `ACTION_FIELDS_INVALID` naming the verb as invalid outside a wizard context.

**[UIS-164]** A `create` ActionRef (UIS-160), as a `list-detail` page's `newAction` (UIS-021), realizes the **declarative create idiom**: a host renders the "new" affordance as entering a blank detail form bound to a fresh in-memory record — seeded from `itemDefault` plus the universal-envelope defaults below — that `submit` persists through the same create path (UIS-161). Two OPTIONAL envelope-**seed** fields declare where a new row's required envelope values come from: `scopeFrom` — a Binding (UIS-100), resolved **once** against the page's own enclosing Scope at the moment the draft is seeded (the same once-read discipline a bare-string Binding has, UIS-110 — never a LiveBinding, since a one-time seed has nothing to keep live), whose resolved value seeds the new record's `scope_node`; and `lifecycle` — a **literal** string (UIS-108, never the string-is-Binding rule) seeding the new record's `lifecycle_state` for a draft-publish collection. Both are optional: a page that leaves the new row's scope to the host omits `scopeFrom` (the host supplies the current site, and a `scopeFrom` that resolves to absent at draft time seeds no `scope_node` rather than a null), and a collection that is not draft-publish omits `lifecycle`. Because both degrade **silently** to a host default when absent — a typo or a wrong shape does not fail loud the way a missing required field does — a `create` ActionRef's field set is **closed** to exactly `{verb, target, itemDefault?, scopeFrom?, lifecycle?}` plus the two verb-independent envelope fields UIS-160 defines (`confirm`, UIS-165; `outcomeTo`, UIS-166), which are valid on every verb and are therefore never "unrecognized" here: an unrecognized field (e.g. a misspelled `scopeForm`) MUST fail validation as `ACTION_FIELDS_INVALID`, a `scopeFrom` that is not a syntactically valid Binding string MUST fail as `BINDING_PATH_INVALID` (UIS-066), and a non-string `lifecycle` MUST fail as `ACTION_FIELDS_INVALID` (UIS-160) — never accepted unvalidated on the theory the renderer will fall back to a default at render time (UIS-200). These two fields are the ui-schema/1 minor (Negotiation — a new optional field on an existing shape) that the create idiom relies on.

**[UIS-165]** An ActionRef of ANY verb (UIS-160) MAY declare `confirm` as a **ConfirmSpec**: `{titleMsg, bodyMsg?, confirmLabelMsg?, cancelLabelMsg?, destructive?}`, where `titleMsg` is a REQUIRED `msg:` reference (Definitions), `bodyMsg`/`confirmLabelMsg`/`cancelLabelMsg` are OPTIONAL `msg:` references, and `destructive` is an OPTIONAL boolean (default `false`) declaring that the guarded verb removes or interrupts something rather than merely writing it. When `confirm` is present the renderer MUST NOT dispatch the ActionRef on the widget event alone: it presents the ConfirmSpec, and dispatches **that same ActionRef, unchanged, in that same Scope** only on the operator's explicit acknowledgement. A dismissal — a cancel, an escape, a close — dispatches nothing at all; a confirm an operator can back out of and still have acted is not a confirm.

`confirm` is a field on the ActionRef and not a widget kind, and the difference is load-bearing rather than stylistic. A confirmation has no place in a content tree: it binds to no data, occupies no position in a layout, and exists only for the duration of one event. Expressing it as a widget would mean re-deriving its state machine in every page (`set $ui.confirming` → a `visibleIf`-gated panel → a second `button` carrying the real verb) and — worse — would **decouple the warning from the act**: nothing in the grammar could then hold the sentence an operator read and the verb that ran to be the same thing, and a page whose dialog says "Delete this screen?" over a button wired to a different `target` would be perfectly conformant. As a field, the two are one node: a reader of the document, and a validator, see the warning and the verb together. It also composes with everything already defined — a `confirm` on a `table`'s `rowPress`, on a `wizard`'s `onFinish`, on a `list-detail`'s `newAction` — with no per-site plumbing.

`confirm`'s own field set is **closed** to exactly `{titleMsg, bodyMsg?, confirmLabelMsg?, cancelLabelMsg?, destructive?}`. A `confirm` that is not an object, an unrecognized key inside it (a misspelled `bodyMessage`), an absent or non-string `titleMsg`, or a non-boolean `destructive` MUST each fail validation as `ACTION_FIELDS_INVALID` at the offending field's own path — never accepted unvalidated on the theory the renderer will fall back at render time (UIS-200). `titleMsg` is required for the same reason UIS-075 requires `labelMsg`: the dialog's title is its accessible name, there is no other place for one to come from, and a modal with no accessible name is a control a screen-reader user cannot identify. `confirm` is an OPTIONAL field on an existing shape, so its addition is a ui-schema/1 minor (Negotiation).

**[UIS-166]** An ActionRef whose verb dispatches through the host's own write/dispatch path — `submit`, `create`, `delete`, `call-action` (UIS-161), the **seam verbs** — MAY declare `outcomeTo` as a Binding (UIS-100) naming where that invocation's **ActionOutcome** is published. `outcomeTo` MUST be rooted at `$ui` and MUST name at least one segment beneath it (`$ui.<name>[.<name>…]`, UIS-104); a non-string, a grammar-malformed path, a bare `$ui`, or a path rooted anywhere else MUST fail validation as `BINDING_PATH_INVALID`. The destination is fixed to `$ui` by the same rule that makes `$ui` what it is: an outcome is page-scoped interaction bookkeeping that is never persisted and never rides in a `submit` payload (UIS-104), and permitting it into the bound resource would let a transient refusal be written back to the server as if it were part of the record.

An **ActionOutcome** is `{status, code?, detail?, traceId?, result?}`:

| field | meaning |
|---|---|
| `status` | `pending` while the invocation is in flight, `ok` once it has succeeded, `error` once it has been refused or has failed. Always present. |
| `code` | On `error`, the refusal's own machine-readable code as the host reported it, or `null` when the failure carries none. |
| `detail` | On `error`, the human sentence the host reported. While `pending`, the host's own interim description, if it reported one. |
| `traceId` | On `error`, the failed request's trace identifier, when the host reported one. |
| `result` | On `ok`, the value the invocation produced. While `pending`, the host's own interim value, if it reported one. |

The renderer writes `{status: "pending"}` at `outcomeTo` at the moment it dispatches, replaces it with `{status: "ok", result}` on success, and with `{status: "error", code, detail, traceId}` on failure. A host MAY report interim `detail`/`result` while the invocation is still in flight; the renderer merges those into the pending record and leaves `status` at `pending`, and MUST ignore any such report arriving after the outcome has settled. That progress channel is a property of the host's own dispatch seam and needs no grammar of its own — this contract fixes only the SHAPE a page binds against, exactly as it declines to specify the rendering engine elsewhere (Scope).

This is deliberately the whole of the asynchronous story: there is no `pending` widget, no lifecycle verb, and no separate error-display kind. `status` is ordinary `$ui` state (UIS-104), so `visibleIf` (UIS-063), a `switch`'s `discriminant` (UIS-070), a `button`'s `disabledIf` (UIS-076) and any `text`/`badge` `value` bind to it with the grammar already defined — a page renders "working", "it failed, and here is which refusal" and "it worked" out of constructs this contract has always had. What was missing was never a lifecycle vocabulary; it was that a dispatched verb's result went nowhere a Binding could reach, so a published error code could not be rendered by a schema-authored page **at all**. `outcomeTo` is an OPTIONAL field on an existing shape, so its addition is a ui-schema/1 minor (Negotiation); an ActionRef that omits it dispatches exactly as it always has, fire-and-forget.

**[UIS-167]** `outcomeTo` (UIS-166) is valid ONLY on a seam verb (`submit`, `create`, `delete`, `call-action`). On any other verb — `navigate`, the local verbs `set`/`repeat-add`/`repeat-remove`, or the wizard verbs — it MUST fail validation as `ACTION_FIELDS_INVALID`, rather than being accepted and never settling. Those verbs complete synchronously against the renderer's own in-memory trees (UIS-162) or the enclosing wizard's step controller (UIS-050); they produce no result and cannot be refused, so an outcome binding on one is a declaration with nothing on the other end — a control gated on `pending` that never leaves it, which is precisely the shape a closed grammar exists to reject statically rather than discover at render time (UIS-200).

For the same reason, a declared `outcomeTo` MUST settle even when the host has wired no dispatch for the verb at all. A seam a host left unwired means the invocation did not happen, so the outcome MUST be written as `{status: "error", code: "ACTION_DISPATCH_UNWIRED"}` — never `ok` (which would report an act that did not occur) and never left `pending` (a control that waits forever on a request nobody sent). An ActionRef with no `outcomeTo` keeps the pre-existing behavior — an unwired seam is a silent no-op — because there is nothing on that page asking whether it happened.

### Fragments & slots

**[UIS-180]** A `fragment` widget node (Widget catalog, UIS-070) MUST declare `ref` as a key present in the page document's own `fragments` map (UIS-004); a `ref` that does not resolve MUST fail validation as `FRAGMENT_REF_UNDEFINED`.

**[UIS-181]** A referenced fragment's own widget-node subtree renders in place of the `fragment` node, exactly as if that subtree had been written inline at that position — `fragment` is pure substitution, never a distinct rendering surface (no implicit wrapper element, no independent lifecycle).

**[UIS-182]** A fragment MAY reference itself, directly or transitively through other fragments, to express recursively-shaped data (e.g. `rules/1`'s own `and`/`or`/`not` condition nesting, RUL-100). The rendered recursion depth is bounded by the actual data it is bound to (a leaf condition with no nested children terminates it), not by the document's own schema; a conformant renderer MUST nonetheless enforce a finite recursion-depth ceiling — supporting self-referential recursion to a depth of at least 16 is REQUIRED, and a document (or, more likely, malformed/cyclic bound data) that would render past whatever finite ceiling the renderer enforces MUST fail closed as `FRAGMENT_RECURSION_DEPTH_EXCEEDED` rather than hang or exhaust the call stack.

**[UIS-183]** A `fragment` node's `bind`, when present, is resolved against the fragment node's own enclosing Scope and becomes the new enclosing Scope for every widget node inside the referenced fragment (an ordinary Scope narrowing, the same mechanism `repeat` uses, UIS-107). A `fragment` node with no `bind` is scope-**transparent**: the referenced fragment's contents inherit the invoking node's own enclosing Scope unchanged. Transparency is what makes self-recursion (UIS-182) address the correct, progressively-nested data: a `repeat` bound to a condition array establishes a fresh per-item Scope (UIS-107), and a transparent self-referential `fragment` inside that `itemTemplate` renders against that same per-item Scope with no additional `bind` needed.

**[UIS-184]** A `fragment` node's `params`, when present, is an object of literal or Binding values (Binding-typed entries resolved against the node's own enclosing Scope, at reference time); every entry becomes readable inside the referenced fragment via `$params.<name>` (UIS-106). `params` is a value-passing channel, independent of the Scope-narrowing `bind` does — a fragment MAY use one, the other, both, or neither.

**[UIS-185]** A `slot` widget node (UIS-070) names an insertion point; the host renders, at that position, every other pack's page whose `manifest/1` entry declares `fragment: card` (MAN-061) and whose declaring pack was granted this slot by name (MAN-062's `accepts` check), each sized per its own `sizeHint`. This contract governs only the `slot` node's own placement in the layout and the fact that a `fragment: card` page's rendered content is exactly that page's own document (Page documents) rendered as a self-contained subtree — the eligibility/wiring rule itself is `manifest/1`'s (MAN-062).

**[UIS-186]** `slot` names (the `name` prop) MUST be unique within one page document; a duplicate MUST fail validation as `SLOT_NAME_DUPLICATE`.

### Validation

**[UIS-200]** A conformant validator MUST reject a page document under exactly the closed-vocabulary and grammar-well-formedness rules stated above (page type, UIS-003; widget type, UIS-060; widget props/events, UIS-062; binding syntax, UIS-066/UIS-100; vocabRef membership, UIS-121; OptionSource shape, UIS-130; fragment reference resolution, UIS-180; action verb/fields, UIS-160) — never by silently coercing an unrecognized member into a default rendering, and never by accepting it unvalidated on the theory that the renderer will fail at render time instead. Every rejection MUST report a `code` from Error taxonomy and the offending field's own path (the same dotted/bracket notation Binding grammar: data paths defines), so a driver asserts on `code`, not on message text — the same posture `rules/1`'s own Scope and Conformance notes establish for its vocabulary.

**[UIS-201]** Validation composes with `manifest/1`'s own install-time checks (MAN-010) but is independent of them: a page document is well-formed or not under this contract's own rules regardless of whether the pack manifest declaring it has itself been validated yet — a host MAY validate a bundled page document at pack-build/lint time, ahead of any install.

**[UIS-202]** A `switch` node (UIS-070) with no `case` matching the current `discriminant` value and no `default` renders nothing at that position — this is defined behavior, not a validation failure; an author who needs every value of a vocabRef-sourced discriminant handled achieves it by supplying a `case` per member (UIS-122) or a `default`, not by a validation rule this contract cannot statically prove against dynamically bound data.

## Wire shapes

```json
// PageDocument — list-detail (illustrative skeleton; conformance/fixtures/automation-builder/ is the full worked example)
{
  "pageType": "list-detail",
  "context": {
    "presets": { "collection": "presets" }
  },
  "fragments": {
    "condition-editor": { "type": "text", "props": { "value": "item.type" } }
  },
  "list": {
    "source": "automations",
    "display": {
      "type": "table",
      "props": {
        "source": "automations",
        "columns": [
          { "headerMsg": "msg:automations.list.name", "cell": "item.name" },
          { "headerMsg": "msg:automations.list.mode", "cell": { "compute": "label", "args": ["rules/1:mode", "item.mode"] } }
        ]
      },
      "on": { "rowPress": { "verb": "set", "target": "$ui.selected", "value": "item.id" } }
    }
  },
  "detail": {
    "source": "automations[id=$ui.selected]",
    "emptyMsg": "msg:automations.detail.empty",
    "root": { "type": "text-input", "bind": "name", "props": { "labelMsg": "msg:automations.detail.nameLabel" } }
  },
  "newAction": { "verb": "create", "target": "automations", "itemDefault": { "mode": "single", "enabled": true, "triggers": [{ "type": "state" }], "conditions": [], "actions": [{ "type": "device_command" }] } }
}
```

```json
// WidgetNode — repeat + self-referential fragment (nested and/or condition groups)
{
  "type": "repeat",
  "bind": "conditions",
  "props": {
    "itemTemplate": { "type": "fragment", "props": { "ref": "condition-editor" } }
  }
}
```

```json
// Fragment "condition-editor" — key-presence discriminated union (UIS-142), the and/or/not/leaf split
{
  "type": "switch",
  "props": {
    "discriminant": { "compute": "firstKey", "args": ["item", ["and", "or", "not", "type"]] },
    "cases": [
      { "when": "and", "render": { "type": "repeat", "bind": "item.and", "props": { "itemTemplate": { "type": "fragment", "props": { "ref": "condition-editor" } } } } },
      { "when": "or", "render": { "type": "repeat", "bind": "item.or", "props": { "itemTemplate": { "type": "fragment", "props": { "ref": "condition-editor" } } } } },
      { "when": "not", "render": { "type": "fragment", "bind": "item.not", "props": { "ref": "condition-editor" } } },
      {
        "when": "type",
        "render": {
          "type": "switch",
          "props": {
            "discriminant": "item.type",
            "cases": [
              { "when": "state", "render": { "type": "entity-picker", "bind": "item.entity_id", "props": { "labelMsg": "msg:automations.detail.conditionEntityLabel", "bindShape": "entityId" } } },
              { "when": "numeric", "render": { "type": "number-input", "bind": "item.above", "props": { "labelMsg": "msg:automations.detail.conditionAboveLabel" } } }
            ]
          }
        }
      }
    ]
  }
}
```

```json
// OptionSource — vocab kind
{
  "kind": "vocab",
  "ref": "rules/1:mode",
  "labels": {
    "single": "msg:mode.single",
    "restart": "msg:mode.restart",
    "queued": "msg:mode.queued",
    "parallel": "msg:mode.parallel"
  }
}
```

```json
// OptionSource — data kind (dynamic preset picker)
{ "kind": "data", "source": "$context.presets", "valuePath": "id", "labelPath": "name" }
```

```json
// ActionRef — repeat-add with a typed default (add a state trigger)
{ "verb": "repeat-add", "target": "triggers", "itemDefault": { "type": "state" } }
```

```json
// ContextRef
{ "collection": "presets" }
```

```json
// BindingExpr — locale-formatted date/number display (UIS-143), e.g. a stat-tile value
{ "compute": "formatDate", "args": ["created_at"] }
```

```json
// BindingExpr — locale-formatted currency display (UIS-143), e.g. a table column's cell
{ "compute": "formatCurrency", "args": ["price", "USD"] }
```

```json
// LiveBinding — a dashboard tile that re-evaluates over events/1 instead of fetching once (UIS-109/110)
{ "type": "stat-tile", "props": { "labelMsg": "msg:dashboard.currentTemp", "value": { "path": "current_temperature", "live": true } } }
```

```json
// list.source — keyset-cursor paginated form (UIS-023), api/1 API-030–036
{ "path": "automations", "paginated": true, "limit": 100 }
```

```json
// ActionRef — a confirmed, outcome-publishing invocation (UIS-165/166): the button
// that owns it, the confirmation it is gated behind, and the pending state that
// makes it non-re-entrant (UIS-076)
{
  "type": "button",
  "props": {
    "labelMsg": "msg:system.restart.button",
    "style": "destructive",
    "disabledIf": { "compute": "eq", "args": ["$ui.restart.status", { "lit": "pending" }] }
  },
  "on": {
    "press": {
      "verb": "call-action",
      "action": "system.restart",
      "outcomeTo": "$ui.restart",
      "confirm": {
        "titleMsg": "msg:system.restart.confirm.title",
        "bodyMsg": "msg:system.restart.confirm.body",
        "confirmLabelMsg": "msg:system.restart.confirm.ok",
        "cancelLabelMsg": "msg:system.restart.confirm.cancel",
        "destructive": true
      }
    }
  }
}
```

```json
// Rendering a published refusal code from an ActionOutcome (UIS-166/077) — the
// switch a page uses to turn each code into its own sentence, announced when it lands
{
  "type": "switch",
  "visibleIf": { "compute": "eq", "args": ["$ui.restart.status", { "lit": "error" }] },
  "props": {
    "discriminant": "$ui.restart.code",
    "cases": [
      {
        "when": "RESTART_UNSUPPORTED",
        "render": {
          "type": "text",
          "props": {
            "announce": "assertive",
            "value": { "compute": "msg", "args": ["msg:system.restart.unsupported", "$ui.restart.detail"] }
          }
        }
      }
    ],
    "default": {
      "type": "text",
      "props": { "announce": "assertive", "value": "$ui.restart.detail" }
    }
  }
}
```

## Negotiation

ui-schema/1 has no live wire handshake of its own; a pack's bundled page documents (UIS-001) are read directly by the host renderer at install/render time, the same way `manifest/1` is. Compatibility is expressed at the vocabulary level, mirroring `rules/1`'s own policy exactly: adding a page type, a widget type, a vocabRef (or a member to an existing vocabRef's table), a Computed function, an ActionRef verb, or a new **optional** prop, field, or Binding alternate form (e.g. LiveBinding, UIS-109; `entity-picker`'s `bindShape`, UIS-073a) on an existing widget or shape is a ui-schema/1 minor.

**v1.1** is exactly such a minor, and is recorded here rather than inferred from the diff. It adds four OPTIONAL fields and no new widget type, page type, verb, vocabRef or Computed function: `button.disabledIf` (UIS-076) and `text.announce` (UIS-077), two optional props on existing widgets; and `confirm` (UIS-165) and `outcomeTo` (UIS-166–167), two optional verb-independent ActionRef envelope fields. The widget catalog (UIS-060/070) and the action-verb set (UIS-160) are **unchanged in membership** — a v1.0 document is a valid v1.1 document with identical rendering, and a v1.1 document that declares none of the four is indistinguishable from a v1.0 one. The taxonomy gains one code, `ACTION_DISPATCH_UNWIRED`, which no v1.0 document can provoke (it requires `outcomeTo`), so it widens no existing refusal. Growing the widget catalog to add a confirmation kind, or the verb set to add a lifecycle verb, would have been the other way to close these gaps and was deliberately not taken (UIS-165, UIS-166): a closed catalog is worth more than the convenience, and neither gap turned out to need one. Removing any of those, narrowing an existing widget's accepted `props`/`bind` shape, adding a new **required** prop or field to an existing widget or shape (the discipline UIS-075's own addition follows), or changing an existing widget's or verb's evaluation semantics is a ui-schema/1 major. `manifest/1`'s `compat.renderer` (MAN-010) is how a pack declares which page types (this contract's own closed set, UIS-002) its bundled UI relies on; a host refuses install on a page type it does not implement, exactly as MAN-010 already states from the manifest side.

## Error taxonomy

| code | meaning | retryable |
|---|---|---|
| `UNKNOWN_PAGE_TYPE` | A page document's `pageType` is not a member of Page types (UIS-003). Shared with `manifest/1`'s own use of this code for the corresponding manifest-side check. | no |
| `UNKNOWN_WIDGET_TYPE` | A widget node's `type` is not a member of the Widget catalog (UIS-060). | no |
| `WIDGET_PROP_UNKNOWN` | A widget node's `props` or `on` carries a key its catalog entry does not declare (UIS-062). | no |
| `WIDGET_REQUIRED_FIELD_MISSING` | A widget node omits a `props`/`on` key its catalog entry marks required (UIS-062, UIS-074, UIS-075). | no |
| `WIDGET_EVENT_UNKNOWN` | A widget node's `on` names an event its catalog entry does not declare (UIS-062). | no |
| `BINDING_PATH_INVALID` | A Binding does not match the data-path grammar (UIS-100), targets a read-only reserved segment such as `$index` (UIS-107), or names a destination the field it sits in forbids — an `outcomeTo` not rooted at `$ui.<name>` (UIS-166). | no |
| `BINDING_TYPE_MISMATCH` | A statically-known type mismatch between a widget's bind-shape and the bound field (UIS-066), a bound literal outside a `literal`/`vocab` OptionSource's closed set (UIS-134), or an `entity-picker`'s `bindShape` outside its closed set or `modes` naming a form that `bindShape` cannot carry (UIS-073a). | no |
| `FRAGMENT_REF_UNDEFINED` | A `fragment` node's `ref` does not resolve to a key in the document's own `fragments` (UIS-180). | no |
| `FRAGMENT_RECURSION_DEPTH_EXCEEDED` | Rendering a self-referential fragment chain exceeded the renderer's enforced depth ceiling (UIS-182). | no |
| `SLOT_NAME_DUPLICATE` | Two `slot` nodes in one page document share a `name` (UIS-186). | no |
| `VOCAB_REF_UNKNOWN` | A vocabRef is not a member of the UIS-120 table (UIS-121). | no |
| `OPTION_SOURCE_INVALID` | An `options` prop's `kind` is unrecognized, a kind-specific required field is missing, or a `vocab`-kind `labels` map omits a member (UIS-130–132). | no |
| `COMPUTE_FN_UNKNOWN` | A Computed's `compute` is not a member of the UIS-140 table. | no |
| `CURRENCY_CODE_INVALID` | A `formatCurrency` Computed's `currencyCode` argument is absent, non-string, or not a well-formed ISO 4217 code (UIS-144). | no |
| `CONTEXT_REF_UNDEFINED` | A `$context.<name>` Binding's `<name>` is not a key of the document's own `context` map (UIS-105). | no |
| `ACTION_VERB_UNKNOWN` | An ActionRef's `verb` is not a member of the UIS-160 table. | no |
| `ACTION_FIELDS_INVALID` | An ActionRef is missing a verb-required field, carries a wrongly-shaped one (including a non-string `create` `lifecycle` or an unrecognized `create` field, UIS-164; a malformed `confirm` ConfirmSpec or one missing `titleMsg`, UIS-165), uses a wizard-only verb outside a wizard (UIS-163), or declares `outcomeTo` on a verb that is not a seam verb (UIS-167). | no |
| `ACTION_DISPATCH_UNWIRED` | An ActionRef declaring `outcomeTo` (UIS-166) named a seam verb the host has wired no dispatch for, so the invocation did not happen (UIS-167). Reported as the outcome's own `code`, not as a validation rejection — whether a host wired a seam is not a property of the document. | no |
| `WIZARD_STEP_ID_DUPLICATE` | Two entries in a `wizard` page's `steps` share an `id` (UIS-050). | no |

## Conformance notes

- Traceability map: `conformance/traceability/ui-schema-1.md` — maps every `UIS-NNN` above to the case(s) that exercise it.
- Corpus: `conformance/corpora/ui-schema-1/` — one JSON case file per `case-id`, covering a valid document of each page type, an unknown-widget rejection, a malformed-binding rejection, dedicated cases for the predicate-index selection pattern and a `vocab`-kind OptionSource, and the v1.1 additions: a valid confirmed + outcome-publishing destructive action driven through its dialog to its dispatch, a valid `disabledIf`/`announce` pair driven through pending → refused with a rendered code, and four rejections (a `confirm` missing `titleMsg`, a `confirm` carrying an unrecognized key, an `outcomeTo` outside `$ui`, and an `outcomeTo` on a local verb).
- The automation-builder fixture (`conformance/fixtures/automation-builder/`) is this contract's dedicated stress-test acceptance artifact — a complete, realistic `list-detail` page document expressing an automation-rule editor (trigger add/edit across five trigger kinds, nested `and`/`or`/`not` condition groups via self-referential fragments, an action list including preset-batch selection, and a mode selector with a conditionally-visible `max` field and `for:`/duration inputs) plus a written render-walkthrough asserting every widget and binding construct it uses is defined above. It is a separate mechanism from the `case_id` corpus — verified by its own dependency-free `fixture-lint` script (checked in beside it, asserting every construct it names is defined above) AND by a render driver that paints it against its own sample record through a real renderer (`web/src/renderer/fixture-automation-builder.test.tsx`, asserting the declared structure actually renders), not by the traceability map's per-requirement coverage rows. The two are complementary: the lint checks that the document only names things this contract defines; the render driver checks that a renderer implementing this contract accepts it.
- The `Automation` resource shape (`api/openapi.yaml`) — `id`, `name`, `scope_node`, `labels`, `enabled`, `mode`, `max`, `triggers`, `conditions`, `actions`, `revision`, `created_at`, `updated_at` — is the concrete record the automation-builder fixture's `detail.source` binds to; the fixture's field names (`name`, `mode`, `max`, `triggers`, `conditions`, `actions`) are taken from that schema exactly, not invented in parallel.
- Cross-entity dynamic option population (e.g. constraining a `state` trigger's `to`/`from` picker to the specific device class of whatever entity a sibling field currently names) is intentionally not covered by this version's OptionSource grammar (Binding grammar: option sources) — the automation-builder fixture's own state-trigger case uses a `literal` OptionSource populated for its one worked entity's device class instead.

*draft-note: a reactive cross-field-dependent OptionSource (state/attribute options that update as the referenced entity's device class changes — distinct from the LiveBinding of UIS-109, which governs a single bound value's push delivery over `events/1`, not an OptionSource's candidate-list recomputation) is a plausible near-term ui-schema/1 minor — proposed shape: a fourth OptionSource kind, `registry`, naming the device-class registry and a sibling Binding to resolve the class from, resolved host-side rather than by this contract's own grammar. Not required by the automation-builder fixture (which uses a `literal` source for its one worked example), so not committed here.*

*draft-note: whether `fragments`/`context` should also be declarable once per pack and shared across multiple page documents (rather than duplicated per document, UIS-004) is left open — the automation-builder fixture needs only page-local fragments, so this version scopes both to the single document, extensible additively to a pack-level declaration later without changing any per-document meaning.*

*draft-note: the minimum required self-referential fragment recursion depth (UIS-182) is proposed at 16 — generous headroom over realistic condition-group nesting (the fixture itself nests three deep) but not derived from a measured ceiling; confirm before this contract leaves draft.*
