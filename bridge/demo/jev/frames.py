"""Lets Jev see and use the controls inside a page's frames.

Jev reads the top document only, so it never sees a CAPTCHA's checkbox or
image grid: both sit in frames. FrameBrowser runs Jev's own DOM snapshot in
each frame that is a direct child of the page, in an isolated world so the
frame's scripts cannot see it, and adds the frame's controls to the page's
element table at their position on the page. Cross-site frames run in their
own process and are reached through their own CDP target; same-site frames
share the page's.

Unlike the page's own controls, a frame's are offered even while the frame
is scrolled out of view, so Jev knows a CAPTCHA is there. One that something
covers on screen, such as a CAPTCHA's checkbox under its open challenge, is
left out: Jev would keep choosing it and the executor keep refusing it. So is
a checked checkbox, such as a passed CAPTCHA's: Jev would keep clicking it. A frame control is
clicked or typed into like a top-document one, after its frame is scrolled
into view: its geometry is read again and hit-tested inside the frame, the
frame element is hit-tested on the page, and trusted CDP input goes to the
page at that point, which Chrome routes into the frame. Dropdowns inside
frames are left out.
"""

import json
import sys

from browser_harness.helpers import cdp
from jev_ultrafast.browser import READ_STATE, Browser, StalePage, fingerprint

# Frame controls get node ids above the page's: frame k's node n is k * FRAME_NODES + n.
FRAME_NODES = 1_000_000
WORLD = "overpass-jev"

# The frame element's content box on the page, and whether it is shown.
OWNER_BOX = """function () {
  const r = this.getBoundingClientRect();
  return {x: r.left + this.clientLeft, y: r.top + this.clientTop, w: this.clientWidth, h: this.clientHeight,
    shown: this.checkVisibility({checkOpacity: true, checkVisibilityCSS: true})};
}"""
OWNER_HIT = "function (x, y) { return document.elementFromPoint(x, y) === this; }"
OWNER_SHOW = "function () { this.scrollIntoView({block: 'center'}); }"
# Where to click a frame node, in the frame's coordinates, or null if it is
# gone, disabled, hidden or covered. Mirrors Jev's own executor.
FRAME_TARGET = """(node => {
  const e = window.__jevFast?.nodes.get(node);
  if (!e?.isConnected || e.matches(':disabled') || e.closest('[aria-disabled="true"],[inert]') ||
      !e.checkVisibility({checkOpacity: true, checkVisibilityCSS: true})) return null;
  const r = e.getBoundingClientRect(), x = r.x + r.width / 2, y = r.y + r.height / 2;
  if (!r.width || !r.height || x < 0 || y < 0 || x >= innerWidth || y >= innerHeight) return null;
  if (!e.contains(document.elementFromPoint(x, y))) return null;
  return {x, y};
})(%d)"""
FRAME_GUARD = "(() => { const c = window.__jevFast; return c ? c.guard(c.nodes.get(%d)) : null; })()"
# Which of the listed nodes, at the given points in the frame, something else covers.
FRAME_COVERED = """(points => points.filter(([node, x, y]) => {
  const e = window.__jevFast?.nodes.get(node);
  return !e || !e.contains(document.elementFromPoint(x, y));
}).map(([node]) => node))(%s)"""


class FrameBrowser(Browser):
    """Jev's Browser, with frame controls in its observations."""

    on_hire_human = None  # set by the driver: called with the page when Jev chooses HIRE_HUMAN
    hire_label = None  # the HIRE_HUMAN operation's description, or None to leave it out

    def __init__(self, url):
        self.frame_ids = {}  # frame id -> k, stable for the run
        self.frame_sessions = {}  # frame id -> CDP session of its target
        self.worlds = {}  # frame id -> isolated world's execution context
        super().__init__(url)

    # Frames

    def frames(self):
        """The page's direct child frames, as (frame id, CDP session) pairs."""
        tree = self.call("Page.getFrameTree")["frameTree"]
        found = {c["frame"]["id"]: self.session for c in tree.get("childFrames", [])}
        for t in cdp("Target.getTargets", filter=[{"type": "iframe"}])["targetInfos"]:
            if t["targetId"] in found:
                continue
            if t["targetId"] not in self.frame_sessions:
                try:
                    self.frame_sessions[t["targetId"]] = cdp("Target.attachToTarget", targetId=t["targetId"],
                                                             flatten=True)["sessionId"]
                except RuntimeError:
                    continue  # gone already
            found[t["targetId"]] = self.frame_sessions[t["targetId"]]
        return list(found.items())

    def frame_eval(self, frame_id, session, expression):
        """Evaluates in the frame's isolated world, made again if the frame navigated."""
        for attempt in range(2):
            if frame_id not in self.worlds:
                self.worlds[frame_id] = cdp("Page.createIsolatedWorld", session_id=session, frameId=frame_id,
                                            worldName=WORLD)["executionContextId"]
            try:
                result = cdp("Runtime.evaluate", session_id=session, contextId=self.worlds[frame_id],
                             expression=expression, returnByValue=True)
            except RuntimeError:
                self.worlds.pop(frame_id, None)  # the context went with a navigation
                continue
            if result.get("exceptionDetails"):
                raise StalePage("Frame changed during evaluation")
            return result.get("result", {}).get("value")
        raise StalePage("Frame is navigating")

    def owner(self, frame_id, function, *args):
        """Calls function on the frame's element in the page; None if it is not in the top document."""
        try:
            node = self.call("DOM.getFrameOwner", frameId=frame_id)["backendNodeId"]
            obj = self.call("DOM.resolveNode", backendNodeId=node)["object"]["objectId"]
            result = self.call("Runtime.callFunctionOn", objectId=obj, functionDeclaration=function,
                               arguments=[{"value": a} for a in args], returnByValue=True)
        except RuntimeError:
            return None  # a nested frame, or one that is gone
        return result.get("result", {}).get("value")

    def frame_of(self, action):
        return action["frame"], self.frame_sessions.get(action["frame"], self.session)

    # Observation

    def observe(self, screenshot=True):
        page = super().observe(screenshot=screenshot)
        controls = [a for a in page["actions"] if a["kind"] in {"scroll", "wait"}]
        actions = [a for a in page["actions"] if a["kind"] not in {"scroll", "wait"}]
        texts = [page["text"]]
        for frame_id, session in self.frames():
            box = self.owner(frame_id, OWNER_BOX)
            if not box or not box["shown"] or box["w"] <= 0 or box["h"] <= 0:
                continue
            try:
                state = self.frame_eval(frame_id, session, READ_STATE)
            except StalePage:
                continue
            if not state:
                continue
            k = self.frame_ids.setdefault(frame_id, len(self.frame_ids) + 1)
            covered = self.covered(frame_id, session, box, page, state["actions"])
            for a in state["actions"]:
                if a["kind"] not in {"click", "fill"} or a["node"] in covered:
                    continue
                if a.get("role") == "checkbox" and a.get("checked") == "true":
                    continue  # e.g. a passed CAPTCHA: clicking it again does nothing
                r = a["rect"]
                node = k * FRAME_NODES + a["node"]
                actions.append({**a, "id": f"f{k}{a['id']}", "node": node, "frame": frame_id, "frame_node": a["node"],
                                "rect": {**r, "x": box["x"] + r["x"], "y": box["y"] + r["y"]}})
                page["guards"][str(node)] = state["guards"].get(str(a["node"]))
            if state["text"]:
                texts.append(f"[In a frame: {state['title'] or state['url'].split('?')[0]}]\n{state['text']}")
        if self.hire_label:
            controls.insert(0, {"id": "hire_human", "kind": "hire_human", "label": self.hire_label})
        page["actions"] = actions + controls
        page["text"] = "\n\n".join(texts)[:8000]
        page["fingerprint"] = fingerprint(page)
        return page

    def covered(self, frame_id, session, box, page, actions):
        """Nodes of a frame's on-screen controls that something covers, in the frame or on the page."""
        points, covered = [], set()
        for a in actions:
            r = a.get("rect")
            if not r:
                continue
            x, y = r["x"] + r["w"] / 2, r["y"] + r["h"] / 2
            if not (0 <= box["x"] + x < page["w"] and 0 <= box["y"] + y < page["h"]):
                continue  # off screen: shown into view before it is clicked
            if not self.owner(frame_id, OWNER_HIT, box["x"] + x, box["y"] + y):
                covered.add(a["node"])
            else:
                points.append([a["node"], x, y])
        if points:
            try:
                covered.update(self.frame_eval(frame_id, session, FRAME_COVERED % json.dumps(points)) or [])
            except StalePage:
                pass
        return covered

    # Execution

    def fresh(self, page, action=None):
        if action is not None and "frame" in action:
            frame_id, session = self.frame_of(action)
            try:
                guard = self.frame_eval(frame_id, session, FRAME_GUARD % action["frame_node"])
            except StalePage:
                return False
            return guard == page["guards"].get(str(action["node"]))
        return super().fresh(page, action)

    def act(self, action, page, text=None):
        if action["kind"] == "hire_human":
            self.after_input = None
            return self.on_hire_human(action, page)
        if "frame" not in action:
            return super().act(action, page, text=text)
        if not self.fresh(page, action):
            raise StalePage("Page changed since this decision. Observe again.")
        frame_id, session = self.frame_of(action)
        self.owner(frame_id, OWNER_SHOW)
        at = self.frame_eval(frame_id, session, FRAME_TARGET % action["frame_node"])
        box = self.owner(frame_id, OWNER_BOX)
        if not at or not box:
            raise StalePage("Target changed or is covered. Observe again.")
        x, y = box["x"] + at["x"], box["y"] + at["y"]
        if not self.owner(frame_id, OWNER_HIT, x, y):
            raise StalePage("The frame is covered at the target. Observe again.")
        for event in ("mousePressed", "mouseReleased"):
            self.call("Input.dispatchMouseEvent", type=event, x=x, y=y, button="left", clickCount=1)
        if action["kind"] == "fill":
            modifiers = 4 if sys.platform == "darwin" else 2
            self.call("Input.dispatchKeyEvent", type="keyDown", key="a", code="KeyA", modifiers=modifiers,
                      commands=["selectAll"])
            self.call("Input.dispatchKeyEvent", type="keyUp", key="a", code="KeyA", modifiers=modifiers)
            self.call("Input.insertText", text=text)
        self.after_input = None
        return {"executed": action["id"]}
