# ALT

> **Pre-release software.** ALT is under active verification. Its storage format, command surface, and runtime contracts may still change before the first stable release.

ALT is a local orchestration environment for people who want to choose how several large language models work together. A Team binds exact gateway models to user-written assignments, an explicit Router, Router-to-Lead authority, stateless-call edges, and stateful-peer edges. The Router selects the Lead responsible for each request; that Lead coordinates any further work and remains accountable for the final answer.

ALT does not choose a default Team, rewrite assignment definitions, substitute missing models, or maintain a hardcoded universal model catalog. The user defines the organization. ALT enforces the organization that was defined.

## Execution model

A published Team revision contains one Router and at least two Leads. Each member has one exact model identity, one verbatim assignment definition, and one exclusive authority role: coordination Lead or contributor. A Router edge makes a member a Lead; removing that edge revokes the authority. Contributors may be available through stateless-call edges, stateful-peer edges, or both, but never become Leads during those relationships.

Routing assigns ownership; it does not precompute the solution. The selected Lead repeatedly decides whether to act directly, call one or more connected members, wait for active work, cancel obsolete work, or answer. Independent calls may run concurrently. Dependent calls are formed after their prerequisites return, so later work can incorporate earlier evidence instead of following a frozen queue.

A specialist called by a Lead starts without conversation history or inherited authority. It receives its assignment definition, the bounded objective, and the context deliberately prepared for that invocation, then returns once. A peer collaboration instead retains only that relationship's evolving context across turns so the Lead and contributor can iteratively refine the work; its state cannot bleed into another collaboration. Neither relationship can answer the user, take ownership, or become a second Router. Sessions retain durable, labelled orchestration history so later turns can continue the same conversation without silently changing the pinned Team revision.

## Context continuity

ALT treats every model context as a bounded projection, not as durable memory. Event payloads, offloaded tool results, permitted pre-compaction agent transcripts, and every Eino checkpoint version are archived exactly before a working view may replace them. Router, Lead, specialist invocation, peer collaboration, and final synthesis have separate context scopes. Leads can recall their conversation; specialists can recall only their invocation and exact references deliberately shared with them; peers can recall only their collaboration. Provider reasoning is excluded unless the pinned Team disclosure policy permits its persistence.

Working views retain current objectives, instructions, active work, recent evidence, and immutable references. Older material is progressively disclosed through three dynamically discoverable tools: `context_browse` pages exact occurrences when the right query is unknown, `context_search` locates relevant records and offloaded output, and `context_open` pages digest-verifiable exact bytes from one referenced occurrence. Eino's reduction middleware likewise offloads exact tool output before clearing old tool rounds; long agent loops archive their permitted transcript before summarization and attach its address to the continuation brief. Compaction therefore never makes a summary the sole surviving copy or repeatedly summarizes away the source. The archive grows until an explicit retention policy exists; ALT does not buy bounded local storage through silent evidence loss.

## Interfaces

Running `alt` opens the terminal interface. ALT starts without a selected Team and refuses to submit a prompt until the user selects or creates one.

| Command | Effect |
| --- | --- |
| `/profile` | Select an immutable Team revision. |
| `/team [id[@revision]]` | Open the native Team graph; switch among New Team, Edit Team, and Inspect Team inside the window. |
| `/thinking` | Toggle the live execution graph for the active conversation. |
| `/auth` | Configure a gateway or research credential. |
| `/research` | Choose Exa or Linkup for web research. |
| `/resume` | Search and resume durable conversations. |
| `/new` | Start a new conversation. |
| `/rename` | Rename the active conversation. |
| `/copy` | Copy the last answer as Markdown. |
| `/cancel` | Interrupt active orchestration. |

The Team surface and execution graph are floating native windows launched by the same executable. One Team window switches among creation, editing, and read-only inspection while preserving the graph as the common object. Authoring publishes validated revisions. The execution graph consumes the ordered events already stored for recovery; it is a projection of runtime state, not a second orchestration authority.

## Gateways and model identity

ALT integrates with multi-model inference gateways rather than requiring one credential for every model laboratory. The currently registered gateways are ClinePass, OpenCode, ZenMux, Together, and Fireworks. Each adapter owns authentication, catalog discovery, endpoint rules, capability evidence, exact model references, and execution for its service. ClinePass uses Cline account device authorization and rotating account tokens; ALT implements that public protocol directly and does not depend on the Cline program.

The Team builder displays the models returned for one authenticated gateway account. Every model in a Team comes from that gateway, so running the Team requires one gateway credential—not a bundle of provider accounts. Provider neutrality means choosing which supported gateway backs the Team. A selected catalog identity is preserved through publication and execution. ALT does not strip provider prefixes, guess replacement models, invent capability support, or represent unknown prices as zero.

Configure connections interactively with `/auth` or from the command line:

```sh
alt auth set opencode
# ClinePass opens account authorization instead of asking for an API key.
alt auth set cline
alt auth status
alt auth models opencode
alt auth test opencode
alt auth set exa
alt auth set linkup
alt research status
alt research set exa
```

Gateway authentication tests list the authenticated catalog and do not spend model tokens. ClinePass first validates the account token against Cline's authenticated models endpoint, then presents the current pass and free catalog without substituting IDs. Exa and Linkup tests perform one explicit minimal search because successful search authorization cannot be established from a catalog. If exactly one research connection is configured, ALT uses it without ceremony; if both are configured, `/research` or `alt research set` chooses the active one.

Credentials are stored in the operating system credential service when it is available. ALT otherwise reports that it is using a private `0600` fallback file in its data directory. The data directory is selected from `--data-dir`, `$ALT_HOME`, `$XDG_DATA_HOME/alt-v1`, or `~/.local/share/alt-v1`.

## Tools and terminal authority

Runtime tools are product infrastructure, not Team roles. Every Lead, peer, and specialist can discover the complete file, context-recall, search, patch, process, and web-research catalogue. Eino's dynamic ToolSearch middleware initially exposes only discovery, then reveals relevant schemas as the work evolves; Team prose never has to predict a later `grep`, process continuation, exact recall, or source fetch. ALT still governs execution by workspace authority and runtime policy. The current surface includes directory listing, file reading and writing, bounded editing, globbing, text search, context browsing/search/opening, command execution, persistent process input, strict patch application, and provider-backed web research.

Safe mode composes Bubblewrap namespaces, `no_new_privs`, and Landlock ABI V3. Commands can read the host filesystem, write only the session workspace and ALT's private temporary directory, and cannot use the host PID, IPC, or UTS namespaces. Direct network access is isolated. Process sessions belong to the assignment that created them and are terminated with that assignment.

`--dangerously-bypass-approvals-and-sandbox` deliberately bypasses Bubblewrap, Landlock, `no_new_privs`, and configured approval gates. ALT still retains process ownership and credential redaction because those are correctness properties, not sandbox restrictions. Both the terminal and native windows display the active authority state.

Web research is an independently selected connection, never part of Team model identity. Exa provides current semantic/deep discovery, rich content controls, exact multi-URL retrieval, and cited answer cross-checks. Linkup provides fast, standard, or multi-iteration deep discovery, source-bearing structured extraction, exact Markdown retrieval with optional JavaScript rendering, and sourced answer cross-checks. The model sees the same `web_search`, `web_fetch`, and `web_answer` concepts with provider-specific schemas; ALT resolves credentials per call and never places them in a model shell. Search and generated answers remain discovery or corroboration until decisive claims are checked against fetched primary evidence.

## Live execution graph

`/thinking` shows the observable flow of work in real time: request arrival, routing, Lead activity, downward delegations, concurrent and sequential branches, tool calls, returns, cancellation, recovery, and finalization. Dynamic tool discovery appears as compact discovery activity rather than masquerading as substantive work; research calls identify the selected connection and retain complete arguments and results for inspection. Provider-supplied reasoning can be inspected only when the provider returned it and the disclosure policy recorded it. ALT does not manufacture private chain-of-thought.

The projection is derived from typed, ordered, durable events. Causal edges come from recorded relationships rather than insertion order, and a repaint is triggered by applied state rather than a polling animation. This distinction matters when branches overlap: two events occurring near each other are not presented as dependent unless the runtime recorded a dependency.

## Research basis

ALT uses a standalone, physics-first constrained-energy engine. Application semantics remain outside it: ALT translates recorded authority into generic directed relationships and geometric constraints, while the engine minimizes hierarchy, stress, repulsion, overlap, crossing, and stability energies over real node rectangles. Exact separation projection, port placement, obstacle routing, and metric evaluation remain distinct stages because no undifferentiated force law can guarantee them. Discrete SCC, symmetry, and crossing methods supply initialization, auxiliary objectives, diagnostics, and measured refinement; they do not infer causality or authority.

The implementation draws on established work rather than visual analogy alone:

- Sugiyama-style layered drawing supplies the directed hierarchy; Brandes and Köpf's coordinate assignment informs compact alignment; and Gansner, Koren, and North's stress majorization provides a principled continuous objective where relaxation is useful.
- Work on graph symmetry, crossing reduction, edge bundling, and orthogonal obstacle routing informs deterministic ordering, stable equivalent-member placement, route separation, and legibility checks.
- Lamport's happened-before relation distinguishes causality from storage order. Petri-net markings inform the representation of active concurrent work, while van der Aalst's analysis of directly-follows graphs explains why adjacency alone cannot represent concurrency honestly.
- Dapper, Pip, and W3C PROV inform the separation between activities, agents, results, responsibility, and return paths. The execution graph records these relationships without claiming that an LLM is literally a formal transition system.
- The terminal interaction was developed through direct source study of Codex and OpenCode. ALT adopts mechanisms that fit its own product—streamed transcript cells, searchable durable history, interruptible work, command discovery, and terminal-native selection—without importing their single-model assumptions or changing ALT's Team architecture.

The concentrated primary sources, their concrete influence, and the limitations ALT deliberately does not inherit are recorded in [RESEARCH.md](RESEARCH.md).

The reusable mathematics lives in [ALT Physics](https://github.com/ALT-Infra/alt-physics). ALT also maintains a narrow [egui_graph fork](https://github.com/ALT-Infra/egui_graph) for interaction semantics required by the native canvases. Both dependencies retain upstream or standalone history and are consumed at pinned commits.

## Architecture

The application and orchestration runtime are written in Go. Bubble Tea v2 renders the terminal interface; Eino supplies maintained orchestration and model abstractions; SQLite in WAL mode stores Teams, conversations, exact context records, versioned working views, checkpoint history, and recovery state. The native graph windows use Rust, `eframe`, `egui`, and the pinned `egui_graph` fork behind a typed graph/event boundary.

The Linux deliverable is one executable. The Rust GUI is compiled as a static library and linked into the Go binary through cgo. The executable relaunches itself in an internal native-window mode when a graph surface is requested. It does not ship a browser runtime, JavaScript bundle, GUI sidecar, or separate sandbox executable. A verified Bubblewrap fallback is embedded for systems without a trusted installation.

## Build and verification

Building requires current Go and Rust toolchains plus the ordinary Linux development libraries required by `eframe`.

```sh
make test
make race
make linux VERSION=v1.0.0-rc.1
./dist/alt-linux-amd64 --version
./dist/alt-linux-amd64 licenses
```

`make test` runs the Go and native Rust suites. `make race` runs the Go suite with the race detector. `make linux` builds the native library and links the single Linux AMD64 executable. Release artifacts are published only after these checks pass and are explicitly marked as GitHub pre-releases until the storage and runtime contracts are declared stable.

The generated [third-party notices](THIRD_PARTY_NOTICES.md) are derived from the production Go and Rust dependency graphs plus the embedded Bubblewrap source. The same notices are embedded in the executable and available through `alt licenses`. Research clones and build-only tools are not redistributed.

## Non-interactive use

```sh
alt exec --team engineering@1 --cd . "Review this repository"
alt exec --team engineering@1 --quiet "Return only the final result"
alt resume
alt resume --last
alt profile list
alt profile show engineering@1
alt profile validate team.yaml
alt profile import team.yaml
alt session list
alt session show SESSION_ID
alt session replay SESSION_ID
alt completion bash
```

Use `alt --help` or `alt <command> --help` for the complete command surface.
