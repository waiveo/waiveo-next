// The api/1 console client — public surface.
//
// `createApi()` builds the whole typed surface over one client; the individual
// types and error classes are re-exported for pages, tests, and the ui-schema
// data sources that render over these resources.

export {
  ApiClient,
  ApiError,
  RevisionConflictError,
  API_BASE,
  type Problem,
  type ErrorCode,
  type ValidationFieldError,
  type FieldErrors,
  type Read,
  type ApiClientOptions,
} from "./client";

export {
  paginate,
  collectPages,
  type Page,
  type PageFetcher,
} from "./pagination";

export {
  createApi,
  updateWithReview,
  etagForRevision,
  type WaiveoApi,
  type ResourceModule,
  type AutomationsModule,
  type ContentModule,
  type Resource,
  type ListParams,
  type UpdateOutcome,
  type ScopeNode,
  type ScopeNodeCreate,
  type ScopeNodeUpdate,
  type Automation,
  type AutomationCreate,
  type AutomationUpdate,
  type AutomationRunResult,
  type Schedule,
  type ScheduleCreate,
  type ScheduleUpdate,
  type Daypart,
  type DaypartCreate,
  type DaypartUpdate,
  type Playlist,
  type PlaylistCreate,
  type PlaylistUpdate,
  type PlaylistItem,
  type ContentUploadResult,
} from "./resources";
