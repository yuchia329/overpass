# Overpass

Human-in-the-loop unblocking for AI agents. When an Agent hits a challenge it cannot pass (e.g. reCAPTCHA), a human Solver remotely controls the Agent's browser to clear it, paid from the Customer's prepaid balance.

## Language

**Agent**:
The Customer's automated browser program. Not part of Overpass; it only embeds the Bridge.
_Avoid_: bot, client

**Customer**:
The party that owns an Agent and pays for unblocking. Identified by their Solana wallet.
_Avoid_: user, client, account

**Solver**:
The human who remotely operates an Agent's browser to clear a challenge.
_Avoid_: user, worker, operator

**Deposit**:
USDC sent from a Customer's registered wallet to the Overpass service wallet, credited to that Customer's Balance.
_Avoid_: top-up, payment

**Unattributed Deposit**:
A Deposit from a wallet no Customer has registered. Credited automatically if that wallet later registers; otherwise kept, never auto-refunded.
_Avoid_: orphan, unknown payment

**Price**:
The fixed USDC amount held per Task. Same for every Task.
_Avoid_: fee, cost

**Earning**:
The Solver's share (80%) of a captured Hold.
_Avoid_: payout, reward

**Fee**:
Overpass's share (20%) of a captured Hold.
_Avoid_: commission, cut

**Balance**:
The Customer's funds held by Overpass; split into available and held.
_Avoid_: wallet, credit

**Hold**:
Funds reserved from a Customer's available Balance for one Task; later captured (Task solved) or released (Task not solved).
_Avoid_: deduction, charge, refund

**Challenge**:
The obstacle blocking an Agent's page that needs a human (reCAPTCHA, slider puzzle, etc.). The Agent may describe it in one sentence when it creates the Task (the Task's `obstacle`); Solvers see that before they Claim, and it is the whole scope of their work: the Agent does the rest itself.
_Avoid_: captcha (too narrow), blocker

**Session**:
The live remote control of one Agent's browser by the Solver who claimed its Task: the Solver sees the page and sends pointer and wheel input.
_Avoid_: remote desktop, connection, stream

**Task**:
One request from an Agent to have a challenge cleared by a Solver. Ends as exactly one of Solved, Expired, or Failed.
_Avoid_: job, ticket, request

**Queue**:
The set of unclaimed Tasks shown live to all Solvers. First Solver to Claim wins.
_Avoid_: pool, backlog, inbox

**Claim**:
A Solver taking ownership of a queued Task. A Task is claimed at most once; it is never requeued.
_Avoid_: pickup, assign

**Claim window**:
How long a Task may wait in the Queue before it Expires.

**Solve window**:
How long a Solver has after Claim before the Task Fails.

**Done**:
The Solver's report that the Challenge is cleared. The Agent takes the page back and checks it with its verify check; if the Challenge is still there, the Solver is told so and keeps the page.

**Solved**:
Task outcome where the Bridge confirmed the Challenge is cleared: by the Agent's cleared check (reCAPTCHA check by default), or by its verify check after the Solver's Done. The only outcome that captures the Hold.

**Expired**:
Task outcome where no Solver claimed it within the claim window.
_Avoid_: timed out, cancelled

**Failed**:
Task outcome where a claimed Task was not Solved within the solve window, the Solver gave up, or the Agent's Bridge disconnected after joining the Session (whether or not the Task was claimed yet). Whichever outcome is recorded first is final.
_Avoid_: timed out, rejected

**Bridge**:
The Overpass TypeScript SDK running inside the Agent's process. It hands the blocked browser page to Overpass and returns once the page is unblocked.
_Avoid_: SDK, client, plugin
