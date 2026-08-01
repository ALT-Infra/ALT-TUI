# ALT

> **Pre-release software.** ALT is under active verification. Its storage format, command surface, and runtime contracts may still change before the first stable release.

ALT is a local orchestration environment for people who want to choose how several large language models work together. A Team binds exact gateway models to user-written assignments, an explicit Router, Lead eligibility, and permitted call edges. The Router selects the Lead responsible for each request; that Lead coordinates any further work and remains accountable for the final answer.

ALT does not choose a default Team, rewrite assignment definitions, substitute missing models, or maintain a hardcoded universal model catalog. The user defines the organization. ALT enforces the organization that was defined.

## Execution model

A published Team revision contains one Router and at least two Lead-capable members. Each member has one exact model identity, one verbatim assignment definition, and an explicit place in the call graph. A model may be both Lead-capable and callable by another Lead without being duplicated into several fictional identities.

Routing assigns ownership; it does not precompute the solution. The selected Lead repeatedly decides whether to act directly, call one or more connected members, wait for active work, cancel obsolete work, or answer. Independent calls may run concurrently. Dependent calls are formed after their prerequisites return, so later work can incorporate earlier evidence instead of following a frozen queue.

A member called by a Lead starts without conversation history or inherited authority. It receives its assignment definition, the bounded objective, the context deliberately prepared for that invocation, and only the runtime tools required for that work. It returns to the Lead and cannot answer the user or become a second Router. Sessions retain durable, labelled orchestration history so later turns can continue the same conversation without silently changing the pinned Team revision.

## Interfaces

Running `alt` opens the terminal interface. ALT starts without a selected Team and refuses to submit a prompt until the user selects or creates one.

| Command | Effect |
| --- | --- |
| `/profile` | Select an immutable Team revision. |
| `/team new` | Open the native Team graph builder. |
| `/team edit [id[@revision]]` | Open a published Team as a mutable draft. |
| `/team` | Inspect the active Team in a read-only native graph. |
| `/thinking` | Toggle the live execution graph for the active conversation. |
| `/auth` | Configure a gateway or Exa credential. |
| `/resume` | Search and resume durable conversations. |
| `/new` | Start a new conversation. |
| `/rename` | Rename the active conversation. |
| `/copy` | Copy the last answer as Markdown. |
| `/cancel` | Interrupt active orchestration. |

The builder, inspector, and execution graph are floating native windows launched by the same executable. The builder writes drafts and publishes validated revisions. The Team inspector is read-only. The execution graph consumes the ordered events already stored for recovery; it is a projection of runtime state, not a second orchestration authority.

## Gateways and model identity

ALT integrates with multi-model inference gateways rather than requiring one credential for every model laboratory. The currently registered gateways are OpenCode, ZenMux, Together, and Fireworks. Each adapter owns authenticated catalog discovery, endpoint rules, capability evidence, exact model references, and execution for its service.

The Team builder displays the models returned for the user's authenticated accounts. A selected catalog identity is preserved through publication and execution. ALT does not strip provider prefixes, guess replacement models, invent capability support, or represent unknown prices as zero.

Configure connections interactively with `/auth` or from the command line:

```sh
alt auth set opencode
alt auth status
alt auth models opencode
alt auth test opencode
alt auth set exa
```

Gateway authentication tests list the authenticated catalog and do not spend model tokens. The Exa test performs one explicit minimal search because successful search authorization cannot be established from a model catalog.

Credentials are stored in the operating system credential service when it is available. ALT otherwise reports that it is using a private `0600` fallback file in its data directory. The data directory is selected from `--data-dir`, `$ALT_HOME`, `$XDG_DATA_HOME/alt-v1`, or `~/.local/share/alt-v1`.

## Tools and terminal authority

Runtime tools are product infrastructure, not Team roles. Every eligible model can be given the same file, search, patch, process, and web-research capabilities when an invocation requires them. The current surface includes directory listing, file reading and writing, bounded editing, globbing, text search, command execution, persistent process input, strict patch application, and Exa-backed web research.

Safe mode composes Bubblewrap namespaces, `no_new_privs`, and Landlock ABI V3. Commands can read the host filesystem, write only the session workspace and ALT's private temporary directory, and cannot use the host PID, IPC, or UTS namespaces. Direct network access is isolated. Process sessions belong to the assignment that created them and are terminated with that assignment.

`--dangerously-bypass-approvals-and-sandbox` deliberately bypasses Bubblewrap, Landlock, `no_new_privs`, and configured approval gates. ALT still retains process ownership and credential redaction because those are correctness properties, not sandbox restrictions. Both the terminal and native windows display the active authority state.

`web_search` uses a separate Exa credential for semantic search, retrieved page contents, and exact-URL retrieval. The credential is resolved by ALT for each call and is never placed in a model's shell environment.

## Live execution graph

`/thinking` shows the observable flow of work in real time: request arrival, routing, Lead activity, downward delegations, concurrent and sequential branches, tool calls, returns, cancellation, recovery, and finalization. Provider-supplied reasoning can be inspected only when the provider returned it and the disclosure policy recorded it. ALT does not manufacture private chain-of-thought.

The projection is derived from typed, ordered, durable events. Causal edges come from recorded relationships rather than insertion order, and a repaint is triggered by applied state rather than a polling animation. This distinction matters when branches overlap: two events occurring near each other are not presented as dependent unless the runtime recorded a dependency.

## Research basis

ALT's graph engine combines discrete graph structure with continuous geometry. Directed layers, strongly connected components, symmetry classes, crossing order, node dimensions, free boundary ports, obstacle corridors, and stable motion are treated as separate constraints rather than collapsed into one force-directed heuristic. Physical energy models are useful for relaxing geometry after the topology is known; they are not used to infer causality or authority.

The implementation draws on established work rather than visual analogy alone:

- Sugiyama-style layered drawing supplies the directed hierarchy; Brandes and Köpf's coordinate assignment informs compact alignment; and Gansner, Koren, and North's stress majorization provides a principled continuous objective where relaxation is useful.
- Work on graph symmetry, crossing reduction, edge bundling, and orthogonal obstacle routing informs deterministic ordering, stable equivalent-member placement, route separation, and legibility checks.
- Lamport's happened-before relation distinguishes causality from storage order. Petri-net markings inform the representation of active concurrent work, while van der Aalst's analysis of directly-follows graphs explains why adjacency alone cannot represent concurrency honestly.
- Dapper, Pip, and W3C PROV inform the separation between activities, agents, results, responsibility, and return paths. The execution graph records these relationships without claiming that an LLM is literally a formal transition system.
- The terminal interaction was developed through direct source study of Codex and OpenCode. ALT adopts mechanisms that fit its own product—streamed transcript cells, searchable durable history, interruptible work, command discovery, and terminal-native selection—without importing their single-model assumptions or changing ALT's Team architecture.

Primary sources include [Lamport's distributed-systems ordering paper](https://www.microsoft.com/en-us/research/publication/time-clocks-ordering-events-distributed-system/), [Brandes and Köpf on fast coordinate assignment](https://boriskoepf.de/papers/gd01a.pdf), [stress majorization in graph drawing](https://www.graphviz.org/documentation/GKN04.pdf), [Pip](https://www.usenix.org/conference/nsdi-06/presentation/pip-detecting-unexpected-distributed-systems), [Dapper](https://research.google.com/archive/papers/dapper-2010-1.pdf), the [W3C PROV primer](https://www.w3.org/TR/prov-primer/), [Petri-net theory](https://docenti.ing.unipi.it/~a009435/issw/extra/murata.pdf), and [Graph of Trace](https://aclanthology.org/2026.acl-demo.29/).

ALT maintains a narrow [egui_graph fork](https://github.com/ALT-Systems/egui_graph) for interaction semantics, reflection-symmetric boundary ports, and obstacle-safe routes required by the native canvases. The fork retains upstream history and is consumed at a pinned commit.

## Architecture

The application and orchestration runtime are written in Go. Bubble Tea v2 renders the terminal interface; Eino supplies maintained orchestration and model abstractions; SQLite in WAL mode stores Teams, conversations, events, checkpoints, and recovery state. The native graph windows use Rust, `eframe`, `egui`, and the pinned `egui_graph` fork behind a typed graph/event boundary.

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
