// Maps the Solver's input from the displayed frame to the Agent's viewport.
//
// The Queue page sends positions normalized to 0–1 of the frame it shows, so
// they do not depend on the Solver's screen size. A screencast frame covers
// deviceWidth × deviceHeight device-independent pixels, the page content
// starts offsetTop below its top, and the page is drawn at pageScaleFactor.
// Playwright's mouse takes CSS pixels of the layout viewport.

/** The part of a CDP screencast frame's metadata the mapping uses. */
export interface FrameMetadata {
  deviceWidth: number;
  deviceHeight: number;
  pageScaleFactor: number;
  offsetTop: number;
}

/** Maps a point normalized to the frame to the Agent's viewport, in CSS pixels. */
export function toViewport(md: FrameMetadata, x: number, y: number) {
  return {
    x: round((x * md.deviceWidth) / md.pageScaleFactor),
    y: round((y * md.deviceHeight - md.offsetTop) / md.pageScaleFactor),
  };
}

/** Maps a wheel scroll given in frame widths and heights to CSS pixels. */
export function wheelToViewport(md: FrameMetadata, dx: number, dy: number) {
  return {
    dx: round((dx * md.deviceWidth) / md.pageScaleFactor),
    dy: round((dy * md.deviceHeight) / md.pageScaleFactor),
  };
}

// Hundredths of a pixel are finer than any input device.
function round(v: number) {
  return Math.round(v * 100) / 100;
}
