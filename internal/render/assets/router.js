(function () {
  'use strict';

  const SVG_NS = 'http://www.w3.org/2000/svg';
  const MAX_REFLOW_PASSES = 3;
  const PRINT_MAX_HEIGHT = 685;
  const PRINT_MAX_WIDTH = 1024;
  const PILL_HEIGHT = 15;
  const PILL_PAD_X = 5;
  const PILL_FONT_SIZE = 9;
  const BRIDGE_RADIUS = 4.5;
  const LANE_STEP = 7.5;

  const ASTAR_DX = [1, -1, 0, 0];
  const ASTAR_DY = [0, 0, 1, -1];
  const ASTAR_NDIR = [1, 1, 2, 2];

  const SEMANTIC_STYLES = {
    ingress: { color: '#2563eb', width: 2.0, dash: '' },
    traffic: { color: '#475569', width: 1.6, dash: '' },
    data: { color: '#0d9488', width: 1.6, dash: '' },
    async: { color: '#6366f1', width: 1.6, dash: '' },
    retry: { color: '#d97706', width: 2.0, dash: '6 4' },
    dlq: { color: '#dc2626', width: 2.0, dash: '6 4' },
    cicd: { color: '#7c3aed', width: 1.6, dash: '' },
    peering: { color: '#9333ea', width: 1.8, dash: '6 4' },
    hybrid: { color: '#0891b2', width: 1.8, dash: '5 3' },
    admin: { color: '#64748b', width: 1.5, dash: '' }
  };

  const TEXT_AUDIT_SELECTOR = [
    '.view-kicker', '.view-title', '.view-subtitle', '.legend-item',
    '.stage-banner',
    '.box-kind-pill', '.box-title', '.box-subtitle', '.box-badge',
    '.arch-card-title', '.arch-card-subtitle', '.arch-card-raw', '.member-chip',
    '.arch-card-meta-label', '.arch-card-meta-value',
    '.chip', '.off-page-pill',
    '.gov-title', '.gov-subtitle', '.gov-group-title',
    '.gov-item-name', '.gov-item-meta', '.gov-footer',
    '.view-footnote-item'
  ].join(',');

  const measureCtx = (function () {
    const canvas = document.createElement('canvas');
    const ctx = canvas.getContext('2d');
    if (ctx) {
      ctx.font = '600 ' + PILL_FONT_SIZE + 'px Inter, sans-serif';
    }
    return ctx;
  })();

  function createAStarWorkspace() {
    return {
      stateCap: 0,
      gScore: null,
      parent: null,
      closed: null,
      heapCap: 1024,
      heapSize: 0,
      heapF: new Float64Array(1024),
      heapSt: new Int32Array(1024),
      prepareStates(n) {
        if (n > this.stateCap) {
          this.stateCap = n;
          this.gScore = new Float64Array(n);
          this.parent = new Int32Array(n);
          this.closed = new Uint8Array(n);
        }
        this.gScore.fill(Infinity, 0, n);
        this.parent.fill(-1, 0, n);
        this.closed.fill(0, 0, n);
        this.heapSize = 0;
      },
      growHeap(minCap) {
        let next = this.heapCap;
        while (next < minCap) next *= 2;
        const nf = new Float64Array(next);
        nf.set(this.heapF);
        const ns = new Int32Array(next);
        ns.set(this.heapSt);
        this.heapF = nf;
        this.heapSt = ns;
        this.heapCap = next;
      }
    };
  }

  function heapPush(ws, f, st) {
    if (ws.heapSize >= ws.heapCap) ws.growHeap(ws.heapSize + 1);
    const hf = ws.heapF;
    const hs = ws.heapSt;
    let i = ws.heapSize++;
    hf[i] = f;
    hs[i] = st;
    while (i > 0) {
      const p = (i - 1) >> 1;
      if (hf[p] <= hf[i]) break;
      const tf = hf[p]; hf[p] = hf[i]; hf[i] = tf;
      const ts = hs[p]; hs[p] = hs[i]; hs[i] = ts;
      i = p;
    }
  }

  function heapPop(ws) {
    const hf = ws.heapF;
    const hs = ws.heapSt;
    const topSt = hs[0];
    ws.heapSize--;
    const size = ws.heapSize;
    if (size === 0) return topSt;
    hf[0] = hf[size];
    hs[0] = hs[size];
    let i = 0;
    while (true) {
      const l = (i << 1) + 1;
      const r = l + 1;
      let sm = i;
      if (l < size && hf[l] < hf[sm]) sm = l;
      if (r < size && hf[r] < hf[sm]) sm = r;
      if (sm === i) break;
      const tf = hf[i]; hf[i] = hf[sm]; hf[sm] = tf;
      const ts = hs[i]; hs[i] = hs[sm]; hs[sm] = ts;
      i = sm;
    }
    return topSt;
  }

  function measurePillWidth(label, fontSize = PILL_FONT_SIZE) {
    if (!label) return 0;
    if (measureCtx) {
      measureCtx.font = '600 ' + fontSize + 'px Inter, sans-serif';
      return Math.max(28, Math.ceil(measureCtx.measureText(label).width) + PILL_PAD_X * 2);
    }
    return Math.max(28, Math.ceil(label.length * fontSize * 0.6) + PILL_PAD_X * 2);
  }

  function relRect(el, origin) {
    const r = el.getBoundingClientRect();
    return {
      left: r.left - origin.left,
      top: r.top - origin.top,
      right: r.right - origin.left,
      bottom: r.bottom - origin.top,
      width: r.width,
      height: r.height,
      cx: r.left - origin.left + r.width / 2,
      cy: r.top - origin.top + r.height / 2
    };
  }

  function rectsOverlap(a, b, pad = 0) {
    return (
      a.left < b.right + pad - 0.5 &&
      a.right > b.left - pad + 0.5 &&
      a.top < b.bottom + pad - 0.5 &&
      a.bottom > b.top - pad + 0.5
    );
  }

  function boxContains(outer, inner, eps = 0.5) {
    return (
      inner.left >= outer.left - eps &&
      inner.top >= outer.top - eps &&
      inner.right <= outer.right + eps &&
      inner.bottom <= outer.bottom + eps
    );
  }

  function segIntersectsRect(x1, y1, x2, y2, r, pad = 0) {
    const minX = Math.min(x1, x2);
    const maxX = Math.max(x1, x2);
    const minY = Math.min(y1, y2);
    const maxY = Math.max(y1, y2);
    const rL = r.left - pad;
    const rR = r.right + pad;
    const rT = r.top - pad;
    const rB = r.bottom + pad;
    if (rL >= rR || rT >= rB) return false;
    return !(maxX <= rL + 0.5 || minX >= rR - 0.5 || maxY <= rT + 0.5 || minY >= rB - 0.5);
  }

  function lowerBound(arr, val) {
    let lo = 0;
    let hi = arr.length;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (arr[mid] < val) lo = mid + 1;
      else hi = mid;
    }
    return lo;
  }

  function uniqueSortedCoords(vals, tol = 2.5) {
    const sorted = vals.slice().sort((a, b) => a - b);
    const out = [];
    for (const v of sorted) {
      if (out.length === 0 || Math.abs(v - out.at(-1)) > tol) {
        out.push(Math.round(v * 2) / 2);
      }
    }
    return out;
  }

  function buildChannelCoords(cards, headers, boxes, stageW, stageH) {
    const xs = [10, 20, stageW - 20, stageW - 10];
    const ys = [10, 20, stageH - 20, stageH - 10];

    const clampX = v => Math.max(8, Math.min(stageW - 8, v));
    const clampY = v => Math.max(8, Math.min(stageH - 8, v));

    const xIntervals = [];
    const yIntervals = [];
    for (const r of cards) {
      xIntervals.push([r.left, r.right]);
      yIntervals.push([r.top, r.bottom]);
      xs.push(clampX(r.left - 10), clampX(r.left - 20), clampX(r.right + 10), clampX(r.right + 20));
      ys.push(clampY(r.top - 10), clampY(r.top - 18), clampY(r.bottom + 10), clampY(r.bottom + 18));
    }
    for (const h of headers) {
      xs.push(clampX(h.left - 8), clampX(h.right + 8));
      ys.push(clampY(h.top - 8), clampY(h.bottom + 8));
    }
    for (const b of boxes) {
      xs.push(clampX(b.left - 12), clampX(b.right + 12));
      ys.push(clampY(b.top - 12), clampY(b.bottom + 12));
    }

    const xEdges = uniqueSortedCoords(xIntervals.flatMap(([l, r]) => [l, r]), 2);
    for (let i = 0; i + 1 < xEdges.length; i++) {
      const gap = xEdges[i + 1] - xEdges[i];
      if (gap >= 12) {
        xs.push((xEdges[i] + xEdges[i + 1]) / 2);
      }
      if (gap >= 90) {
        xs.push(xEdges[i] + 28, xEdges[i + 1] - 28);
      }
    }

    const yEdges = uniqueSortedCoords(yIntervals.flatMap(([t, b]) => [t, b]), 2);
    for (let i = 0; i + 1 < yEdges.length; i++) {
      const gap = yEdges[i + 1] - yEdges[i];
      if (gap >= 10) {
        ys.push((yEdges[i] + yEdges[i + 1]) / 2);
      }
      if (gap >= 72) {
        ys.push(yEdges[i] + 22, yEdges[i + 1] - 22);
      }
    }

    return {
      xs: uniqueSortedCoords(xs, 6),
      ys: uniqueSortedCoords(ys, 6)
    };
  }

  function markHorizontalRect(grid, r, pad, onH) {
    const rL = r.left - pad;
    const rR = r.right + pad;
    const rT = r.top - pad;
    const rB = r.bottom + pad;
    if (rL >= rR || rT >= rB) return;

    const { gridXs, gridYs, gh, hStride } = grid;
    const gxStart = Math.max(0, lowerBound(gridXs, rL + 0.5) - 1);
    const gyStart = lowerBound(gridYs, rT + 0.5);
    for (let gy = gyStart; gy < gh && gridYs[gy] < rB - 0.5; gy++) {
      const y = gridYs[gy];
      const rowOff = gy * hStride;
      for (let gx = gxStart; gx < hStride && gridXs[gx] < rR - 0.5; gx++) {
        if (segIntersectsRect(gridXs[gx], y, gridXs[gx + 1], y, r, pad)) {
          onH(rowOff + gx);
        }
      }
    }
  }

  function markVerticalRect(grid, r, pad, onV) {
    const rL = r.left - pad;
    const rR = r.right + pad;
    const rT = r.top - pad;
    const rB = r.bottom + pad;
    if (rL >= rR || rT >= rB) return;

    const { gridXs, gridYs, gw, gh, vStride } = grid;
    const gxStart = lowerBound(gridXs, rL + 0.5);
    const gyStart = Math.max(0, lowerBound(gridYs, rT + 0.5) - 1);
    for (let gy = gyStart; gy < gh - 1 && gridYs[gy] < rB - 0.5; gy++) {
      const y1 = gridYs[gy];
      const y2 = gridYs[gy + 1];
      const rowOff = gy * vStride;
      for (let gx = gxStart; gx < gw && gridXs[gx] < rR - 0.5; gx++) {
        const x = gridXs[gx];
        if (segIntersectsRect(x, y1, x, y2, r, pad)) {
          onV(rowOff + gx);
        }
      }
    }
  }

  function markRectOnGrid(grid, r, pad, onH, onV) {
    markHorizontalRect(grid, r, pad, onH);
    markVerticalRect(grid, r, pad, onV);
  }

  function markVerticalTopBand(grid, tb, vFlags) {
    const { gridXs, gridYs, gw, gh, vStride } = grid;
    const gxStart = lowerBound(gridXs, tb.left - 1);
    const gyStart = lowerBound(gridYs, tb.top - 1);
    const rR = tb.right + 1;
    const rB = tb.bottom + 1;
    for (let gy = gyStart; gy < gh - 1 && gridYs[gy + 1] <= rB; gy++) {
      const rowOff = gy * vStride;
      for (let gx = gxStart; gx < gw && gridXs[gx] <= rR; gx++) {
        vFlags[rowOff + gx] |= 2;
      }
    }
  }

  function buildStaticGridMasks(gridXs, gridYs, cardRects, headerRects, boxEntries, boxTopBands) {
    const gw = gridXs.length;
    const gh = gridYs.length;
    const hStride = Math.max(0, gw - 1);
    const vStride = gw;
    const hCount = gh * hStride;
    const vCount = Math.max(0, gh - 1) * vStride;

    const hFlags = new Uint8Array(hCount);
    const vFlags = new Uint8Array(vCount);
    const hCardHit = new Int16Array(hCount).fill(-1);
    const vCardHit = new Int16Array(vCount).fill(-1);

    const indexedBoxes = boxEntries.filter(b => b.id);
    const boxWords = Math.max(1, Math.ceil(indexedBoxes.length / 32));
    const hBoxMask = new Uint32Array(hCount * boxWords);
    const vBoxMask = new Uint32Array(vCount * boxWords);
    const grid = { gridXs, gridYs, gw, gh, hStride, vStride };

    for (let ci = 0; ci < cardRects.length; ci++) {
      markRectOnGrid(
        grid,
        cardRects[ci],
        4,
        idx => { hCardHit[idx] = hCardHit[idx] === -1 ? ci : -2; },
        idx => { vCardHit[idx] = vCardHit[idx] === -1 ? ci : -2; }
      );
    }

    for (const h of headerRects) {
      markRectOnGrid(
        grid,
        h,
        3,
        idx => { hFlags[idx] |= 1; },
        idx => { vFlags[idx] |= 1; }
      );
    }

    for (let bi = 0; bi < indexedBoxes.length; bi++) {
      const word = bi >> 5;
      const bit = 1 << (bi & 31);
      markRectOnGrid(
        grid,
        indexedBoxes[bi].stageRect,
        2,
        idx => { hBoxMask[idx * boxWords + word] |= bit; },
        idx => { vBoxMask[idx * boxWords + word] |= bit; }
      );
    }

    for (const tb of boxTopBands) {
      markHorizontalRect(grid, tb, 1, idx => { hFlags[idx] |= 2; });
      markVerticalTopBand(grid, tb, vFlags);
    }

    return {
      gridXs,
      gridYs,
      hStride,
      vStride,
      hFlags,
      vFlags,
      hCardHit,
      vCardHit,
      indexedBoxes,
      boxWords,
      hBoxMask,
      vBoxMask
    };
  }

  function mapCoordsToBaseGrid(coords, baseCoords) {
    const map = new Int32Array(coords.length).fill(-1);
    let gi = 0;
    for (let i = 0; i < coords.length && gi < baseCoords.length; i++) {
      const v = coords[i];
      while (gi < baseCoords.length && baseCoords[gi] < v) gi++;
      if (gi < baseCoords.length && baseCoords[gi] === v) {
        map[i] = gi;
      }
    }
    return map;
  }

  function nearestIdx(arr, val) {
    let best = 0;
    let bestD = Infinity;
    for (let i = 0; i < arr.length; i++) {
      const d = Math.abs(arr[i] - val);
      if (d < bestD) {
        bestD = d;
        best = i;
      }
    }
    return best;
  }

  function edgeStyle(e) {
    const tag = (e.semantic_tag || '').toLowerCase();
    if (SEMANTIC_STYLES[tag]) {
      return { tag, ...SEMANTIC_STYLES[tag] };
    }
    return { tag: 'traffic', ...SEMANTIC_STYLES.traffic };
  }

  function isPointRect(r) {
    return r.width === 0 && r.height === 0;
  }

  function sideFacingPoint(pt, box) {
    if (pt.cy > box.bottom + 40) return 'bottom';
    if (pt.cy < box.top - 40) return 'top';
    return pt.cx < box.left ? 'left' : 'right';
  }

  function retryOrBackwardSidePair(s, t) {
    if (Math.abs(s.cy - t.cy) < 40) {
      return { outSide: 'bottom', inSide: 'bottom' };
    }
    return s.cy <= t.cy
      ? { outSide: 'bottom', inSide: 'top' }
      : { outSide: 'top', inSide: 'bottom' };
  }

  function computePortSidePair(s, t, isRetryOrDLQ) {
    if (isPointRect(s)) {
      return { outSide: 'point', inSide: sideFacingPoint(s, t) };
    }
    if (isPointRect(t)) {
      return { outSide: sideFacingPoint(t, s), inSide: 'point' };
    }
    if (isRetryOrDLQ || s.left > t.right - 8) {
      return retryOrBackwardSidePair(s, t);
    }
    if (s.right <= t.left + 4) return { outSide: 'right', inSide: 'left' };
    if (s.top > t.bottom + 40) return { outSide: 'top', inSide: 'bottom' };
    if (t.top > s.bottom + 40) return { outSide: 'bottom', inSide: 'top' };
    if (Math.abs(s.cx - t.cx) < Math.max(s.width, t.width) * 0.6) {
      return { outSide: 'right', inSide: 'right' };
    }
    return s.cx < t.cx
      ? { outSide: 'right', inSide: 'left' }
      : { outSide: 'left', inSide: 'right' };
  }

  function addMapListEntry(map, key, val) {
    if (!map.has(key)) map.set(key, []);
    map.get(key).push(val);
  }

  function endpointRect(pt, id, nodeRects) {
    if (pt) {
      return { left: pt.x, right: pt.x, top: pt.y, bottom: pt.y, width: 0, height: 0, cx: pt.x, cy: pt.y };
    }
    return nodeRects.get(id);
  }

  function portPointOnSide(r, side, frac, stubOffset) {
    const t = 0.14 + 0.72 * frac;
    if (side === 'right') return { x: r.right, y: r.top + r.height * t, side, stubOffset };
    if (side === 'left') return { x: r.left, y: r.top + r.height * t, side, stubOffset };
    if (side === 'bottom') return { x: r.left + r.width * t, y: r.bottom, side, stubOffset };
    return { x: r.left + r.width * t, y: r.top, side, stubOffset };
  }

  function compareSidePortEntries(a, b, specs, isHorizSide) {
    const oa = a.isOut ? specs[a.idx].t : specs[a.idx].s;
    const ob = b.isOut ? specs[b.idx].t : specs[b.idx].s;
    const diff = isHorizSide ? oa.cx - ob.cx : oa.cy - ob.cy;
    return diff !== 0 ? diff : a.idx - b.idx;
  }

  function assignSidePorts(sidePorts, specs, nodeRects, starts, ends) {
    for (const [key, list] of sidePorts.entries()) {
      const sepIdx = key.lastIndexOf(':');
      const nodeId = key.slice(0, sepIdx);
      const side = key.slice(sepIdx + 1);
      const r = nodeRects.get(nodeId);
      const isHorizSide = side === 'top' || side === 'bottom';
      list.sort((a, b) => compareSidePortEntries(a, b, specs, isHorizSide));
      const count = list.length;
      for (let k = 0; k < count; k++) {
        const entry = list[k];
        const pt = portPointOnSide(r, side, (k + 1) / (count + 1), (k % 3) * 5);
        if (entry.isOut) {
          starts[entry.idx] = pt;
        } else {
          ends[entry.idx] = pt;
        }
      }
    }
  }

  function buildRoutePortSpec(item, idx, nodeRects, sidePorts) {
    const s = endpointRect(item.srcPoint, item.from, nodeRects);
    const t = endpointRect(item.dstPoint, item.to, nodeRects);
    if (!s || !t) return null;
    const { outSide, inSide } = computePortSidePair(s, t, item.is_retry_or_dlq);
    if (outSide !== 'point') addMapListEntry(sidePorts, `${item.from}:${outSide}`, { idx, isOut: true });
    if (inSide !== 'point') addMapListEntry(sidePorts, `${item.to}:${inSide}`, { idx, isOut: false });
    return { outSide, inSide, s, t };
  }

  function computePorts(routeItems, nodeRects) {
    const sidePorts = new Map();
    const specs = routeItems.map((item, idx) => buildRoutePortSpec(item, idx, nodeRects, sidePorts));

    const starts = new Array(routeItems.length);
    const ends = new Array(routeItems.length);
    for (let i = 0; i < routeItems.length; i++) {
      const sp = specs[i];
      if (!sp) continue;
      if (sp.outSide === 'point') {
        starts[i] = { x: sp.s.cx, y: sp.s.cy, side: 'point', isPoint: true, stubOffset: 0 };
      }
      if (sp.inSide === 'point') {
        ends[i] = { x: sp.t.cx, y: sp.t.cy, side: 'point', isPoint: true, stubOffset: 0 };
      }
    }

    assignSidePorts(sidePorts, specs, nodeRects, starts, ends);
    return { starts, ends };
  }

  function stubPoint(pt, dist) {
    if (pt.side === 'right') return { x: pt.x + dist, y: pt.y };
    if (pt.side === 'left') return { x: pt.x - dist, y: pt.y };
    if (pt.side === 'bottom') return { x: pt.x, y: pt.y + dist };
    return { x: pt.x, y: pt.y - dist };
  }

  function isStubClear(pt, s, cardRects, headerRects, ownRect, pad) {
    for (const c of cardRects) {
      if (c !== ownRect && segIntersectsRect(pt.x, pt.y, s.x, s.y, c, pad)) return false;
    }
    for (const h of headerRects) {
      if (segIntersectsRect(pt.x, pt.y, s.x, s.y, h, pad)) return false;
    }
    return true;
  }

  function pickClearStub(pt, baseDist, cardRects, headerRects, ownRect, stageW, stageH) {
    const candidates = [baseDist, 14, 10, 6];
    for (const pad of [4, 1]) {
      for (const d of candidates) {
        const s = stubPoint(pt, d);
        if (s.x < 4 || s.x > stageW - 4 || s.y < 4 || s.y > stageH - 4) continue;
        if (isStubClear(pt, s, cardRects, headerRects, ownRect, pad)) return s;
      }
    }
    return stubPoint(pt, 8);
  }

  function resolveStub(pt, ownRect, routeCtx) {
    if (pt.isPoint) return { x: pt.x, y: pt.y };
    return pickClearStub(
      pt,
      18 + (pt.stubOffset || 0),
      routeCtx.cardRects,
      routeCtx.headerRects,
      ownRect,
      routeCtx.stageW,
      routeCtx.stageH
    );
  }

  function buildNonAncestorMask(indexedBoxes, boxWords, allowedBoxes) {
    const mask = new Uint32Array(boxWords);
    for (let bi = 0; bi < indexedBoxes.length; bi++) {
      if (!allowedBoxes.has(indexedBoxes[bi].id)) {
        mask[bi >> 5] |= 1 << (bi & 31);
      }
    }
    return mask;
  }

  function blockedByCardsSlow(ctx, x1, y1, x2, y2) {
    const { cardRects, srcRect, dstRect } = ctx;
    for (const ob of cardRects) {
      const pad = (ob === srcRect || ob === dstRect) ? -3 : 4;
      if (segIntersectsRect(x1, y1, x2, y2, ob, pad)) return true;
    }
    return false;
  }

  function hitsAnyRect(rects, x1, y1, x2, y2, pad) {
    for (const r of rects) {
      if (segIntersectsRect(x1, y1, x2, y2, r, pad)) return true;
    }
    return false;
  }

  function hitsBoxTopBand(boxTopBands, x1, y1, x2, y2, isH) {
    const minY = Math.min(y1, y2);
    const maxY = Math.max(y1, y2);
    for (const tb of boxTopBands) {
      if (isH) {
        if (segIntersectsRect(x1, y1, x2, y2, tb, 1)) return true;
      } else if (minY >= tb.top - 1 && maxY <= tb.bottom + 1 && x1 >= tb.left - 1 && x1 <= tb.right + 1) {
        return true;
      }
    }
    return false;
  }

  function isCardHitBlocked(ctx, ch, x1, y1, x2, y2) {
    if (ch === -2) return blockedByCardsSlow(ctx, x1, y1, x2, y2);
    if (ch < 0) return false;
    if (ch !== ctx.srcCardIdx && ch !== ctx.dstCardIdx) return true;
    return segIntersectsRect(x1, y1, x2, y2, ctx.cardRects[ch], -3);
  }

  function hasNonAncestorBoxHit(boxMask, segIdx, boxWords, nonAncMask, nonAncMask0) {
    if (boxWords === 1) {
      return (boxMask[segIdx] & nonAncMask0) !== 0;
    }
    const base = segIdx * boxWords;
    for (let w = 0; w < boxWords; w++) {
      if ((boxMask[base + w] & nonAncMask[w]) !== 0) return true;
    }
    return false;
  }

  function resolveBaseGridSegIdx(ctx, x, y, nx, ny, isH) {
    const { xMap, yMap, gridMasks } = ctx;
    if (isH) {
      const ax = Math.min(x, nx);
      const gy = yMap[y];
      const gx = xMap[ax];
      if (gy >= 0 && gx >= 0 && xMap[ax + 1] === gx + 1) {
        return gy * gridMasks.hStride + gx;
      }
      return -1;
    }
    const ay = Math.min(y, ny);
    const gx = xMap[x];
    const gy = yMap[ay];
    if (gx >= 0 && gy >= 0 && yMap[ay + 1] === gy + 1) {
      return gy * gridMasks.vStride + gx;
    }
    return -1;
  }

  function evalGridSegPenalty(ctx, segIdx, isH, x1, y1, x2, y2) {
    const gm = ctx.gridMasks;
    const flags = isH ? gm.hFlags[segIdx] : gm.vFlags[segIdx];
    const ch = isH ? gm.hCardHit[segIdx] : gm.vCardHit[segIdx];
    const boxMask = isH ? gm.hBoxMask : gm.vBoxMask;
    let penalty = 0;
    if ((flags & 1) !== 0 || isCardHitBlocked(ctx, ch, x1, y1, x2, y2)) {
      penalty += 10000000;
    }
    if (hasNonAncestorBoxHit(boxMask, segIdx, gm.boxWords, ctx.nonAncMask, ctx.nonAncMask0)) {
      penalty += 2500;
    }
    if ((flags & 2) !== 0) {
      penalty += 600;
    }
    return penalty;
  }

  function slowSegPenalty(ctx, x1, y1, x2, y2, isH) {
    let penalty = 0;
    if (blockedByCardsSlow(ctx, x1, y1, x2, y2) || hitsAnyRect(ctx.headerRects, x1, y1, x2, y2, 3)) {
      penalty += 10000000;
    }
    if (hitsAnyRect(ctx.nonAncestorBoxes, x1, y1, x2, y2, 2)) {
      penalty += 2500;
    }
    if (hitsBoxTopBand(ctx.boxTopBands, x1, y1, x2, y2, isH)) {
      penalty += 600;
    }
    return penalty;
  }

  function staticSegPenalty(ctx, x, y, nx, ny, isH) {
    const x1 = ctx.xs[x];
    const y1 = ctx.ys[y];
    const x2 = ctx.xs[nx];
    const y2 = ctx.ys[ny];
    const segIdx = resolveBaseGridSegIdx(ctx, x, y, nx, ny, isH);
    if (segIdx >= 0) {
      return evalGridSegPenalty(ctx, segIdx, isH, x1, y1, x2, y2);
    }
    return slowSegPenalty(ctx, x1, y1, x2, y2, isH);
  }

  function classifyHSegInteraction(y1, minX, maxX, s) {
    if (s.isH) {
      return Math.abs(y1 - s.y1) < 2 && Math.min(maxX, s.maxX) > Math.max(minX, s.minX) + 2 ? 1 : 0;
    }
    return s.x1 > minX + 1 && s.x1 < maxX - 1 && y1 > s.minY + 1 && y1 < s.maxY - 1 ? 2 : 0;
  }

  function classifyVSegInteraction(x1, minY, maxY, s) {
    if (!s.isH) {
      return Math.abs(x1 - s.x1) < 2 && Math.min(maxY, s.maxY) > Math.max(minY, s.minY) + 2 ? 1 : 0;
    }
    return x1 > s.minX + 1 && x1 < s.maxX - 1 && s.y1 > minY + 1 && s.y1 < maxY - 1 ? 2 : 0;
  }

  function segInteractionCost(ctx, x1, y1, x2, y2, isH) {
    const { routedSegs, bundleId, isTrunk } = ctx;
    let overlaps = 0;
    let crosses = 0;
    const minX = Math.min(x1, x2);
    const maxX = Math.max(x1, x2);
    const minY = Math.min(y1, y2);
    const maxY = Math.max(y1, y2);
    for (const s of routedSegs) {
      if (bundleId && s.bundleId === bundleId && !isTrunk && !s.isTrunk) continue;
      const kind = isH
        ? classifyHSegInteraction(y1, minX, maxX, s)
        : classifyVSegInteraction(x1, minY, maxY, s);
      if (kind === 1) overlaps++;
      else if (kind === 2) crosses++;
    }
    return overlaps * 28 + crosses * 45;
  }

  function expandAStarNeighbors(ctx, workspace, st, targetX, targetY) {
    const { xs, ys, W, H } = ctx;
    const { gScore, parent, closed } = workspace;
    const dir = st % 3;
    const cell = (st - dir) / 3;
    const x = cell % W;
    const y = (cell - x) / W;
    const x1 = xs[x];
    const y1 = ys[y];
    const baseG = gScore[st];

    for (let di = 0; di < 4; di++) {
      const nx = x + ASTAR_DX[di];
      const ny = y + ASTAR_DY[di];
      if (nx < 0 || nx >= W || ny < 0 || ny >= H) continue;
      const ndir = ASTAR_NDIR[di];
      const nst = (ny * W + nx) * 3 + ndir;
      if (closed[nst]) continue;

      const x2 = xs[nx];
      const y2 = ys[ny];
      const isH = ndir === 1;
      let cost = Math.abs(x2 - x1) + Math.abs(y2 - y1);
      cost += staticSegPenalty(ctx, x, y, nx, ny, isH);
      if (dir !== 0 && dir !== ndir) cost += 35;
      cost += segInteractionCost(ctx, x1, y1, x2, y2, isH);

      const ng = baseG + cost;
      if (ng < gScore[nst]) {
        gScore[nst] = ng;
        parent[nst] = st;
        heapPush(workspace, ng + Math.abs(x2 - targetX) + Math.abs(y2 - targetY), nst);
      }
    }
  }

  function reconstructAStarPath(goalState, parent, xs, ys, sStub, eStub) {
    if (goalState < 0) {
      const midX = (sStub.x + eStub.x) / 2;
      return [sStub, { x: midX, y: sStub.y }, { x: midX, y: eStub.y }, eStub];
    }
    const W = xs.length;
    const pts = [];
    let cur = goalState;
    while (cur >= 0) {
      const dir = cur % 3;
      const cell = (cur - dir) / 3;
      const x = cell % W;
      const y = (cell - x) / W;
      pts.push({ x: xs[x], y: ys[y] });
      cur = parent[cur];
    }
    pts.reverse();
    return pts;
  }

  function routeOrthogonalAStar(pStart, pEnd, routeCtx, edgeCtx) {
    const { gridMasks, cardRects, headerRects, boxTopBands, routedSegs, workspace } = routeCtx;
    const { srcRect, dstRect, nonAncestorBoxes, allowedBoxes, bundleId, isTrunk } = edgeCtx;
    const sStub = resolveStub(pStart, srcRect, routeCtx);
    const eStub = resolveStub(pEnd, dstRect, routeCtx);

    const xs = uniqueSortedCoords([...gridMasks.gridXs, sStub.x, eStub.x], 0.5);
    const ys = uniqueSortedCoords([...gridMasks.gridYs, sStub.y, eStub.y], 0.5);

    const sx = nearestIdx(xs, sStub.x);
    const sy = nearestIdx(ys, sStub.y);
    const tx = nearestIdx(xs, eStub.x);
    const ty = nearestIdx(ys, eStub.y);

    const nonAncMask = buildNonAncestorMask(gridMasks.indexedBoxes, gridMasks.boxWords, allowedBoxes);
    const W = xs.length;
    const H = ys.length;
    const astarCtx = {
      xs,
      ys,
      xMap: mapCoordsToBaseGrid(xs, gridMasks.gridXs),
      yMap: mapCoordsToBaseGrid(ys, gridMasks.gridYs),
      W,
      H,
      srcCardIdx: srcRect ? cardRects.indexOf(srcRect) : -1,
      dstCardIdx: dstRect ? cardRects.indexOf(dstRect) : -1,
      srcRect,
      dstRect,
      cardRects,
      headerRects,
      nonAncestorBoxes,
      boxTopBands,
      gridMasks,
      nonAncMask,
      nonAncMask0: nonAncMask[0],
      routedSegs,
      bundleId,
      isTrunk
    };

    const totalStates = W * H * 3;
    workspace.prepareStates(totalStates);
    const { gScore, parent, closed } = workspace;

    const startState = (sy * W + sx) * 3;
    gScore[startState] = 0;

    const targetX = xs[tx];
    const targetY = ys[ty];
    heapPush(workspace, Math.abs(xs[sx] - targetX) + Math.abs(ys[sy] - targetY), startState);

    let goalState = -1;
    let iters = 0;
    const maxIters = Math.max(120000, totalStates);
    while (workspace.heapSize > 0 && iters < maxIters) {
      iters++;
      const st = heapPop(workspace);
      if (closed[st]) continue;
      closed[st] = 1;

      const cell = (st - (st % 3)) / 3;
      const x = cell % W;
      const y = (cell - x) / W;
      if (x === tx && y === ty) {
        goalState = st;
        break;
      }
      expandAStarNeighbors(astarCtx, workspace, st, targetX, targetY);
    }

    const pts = reconstructAStarPath(goalState, parent, xs, ys, sStub, eStub);
    return simplifyOrthogonal([
      { x: pStart.x, y: pStart.y },
      { x: sStub.x, y: sStub.y },
      ...pts,
      { x: eStub.x, y: eStub.y },
      { x: pEnd.x, y: pEnd.y }
    ]);
  }

  function simplifyOrthogonal(pts) {
    const out = [];
    for (const p of pts) {
      if (out.length === 0) {
        out.push({ x: p.x, y: p.y });
        continue;
      }
      const prev = out.at(-1);
      if (Math.abs(prev.x - p.x) < 0.5 && Math.abs(prev.y - p.y) < 0.5) continue;
      if (out.length >= 2) {
        const p2 = out.at(-2);
        const colH = Math.abs(p2.y - prev.y) < 0.5 && Math.abs(prev.y - p.y) < 0.5;
        const colV = Math.abs(p2.x - prev.x) < 0.5 && Math.abs(prev.x - p.x) < 0.5;
        if (colH || colV) {
          out[out.length - 1] = { x: p.x, y: p.y };
          continue;
        }
      }
      out.push({ x: p.x, y: p.y });
    }
    return out;
  }

  function collectParallelBuckets(allPaths, routeMetaList) {
    const hBuckets = new Map();
    const vBuckets = new Map();
    for (let ei = 0; ei < allPaths.length; ei++) {
      const pts = allPaths[ei];
      const meta = routeMetaList[ei];
      const laneGroup = meta.bundleId && !meta.isTrunk ? meta.bundleId : `e_${ei}`;
      for (let si = 1; si < pts.length - 2; si++) {
        const a = pts[si];
        const b = pts[si + 1];
        if (Math.abs(a.y - b.y) < 0.5) {
          addMapListEntry(hBuckets, Math.round(a.y / 2) * 2, { ei, si, laneGroup, min: Math.min(a.x, b.x), max: Math.max(a.x, b.x) });
        } else if (Math.abs(a.x - b.x) < 0.5) {
          addMapListEntry(vBuckets, Math.round(a.x / 2) * 2, { ei, si, laneGroup, min: Math.min(a.y, b.y), max: Math.max(a.y, b.y) });
        }
      }
    }
    return { hBuckets, vBuckets };
  }

  function isShiftedSegClear(sepCtx, meta, x1, y1, x2, y2) {
    const { srcRect, dstRect, nonAncestorBoxes } = meta;
    for (const ob of sepCtx.cardRects) {
      const pad = (ob === srcRect || ob === dstRect) ? -2 : 4;
      if (segIntersectsRect(x1, y1, x2, y2, ob, pad)) return false;
    }
    if (hitsAnyRect(sepCtx.headerRects, x1, y1, x2, y2, 3)) return false;
    if (hitsAnyRect(nonAncestorBoxes, x1, y1, x2, y2, 2)) return false;
    if (Math.abs(y1 - y2) < 0.5 && hitsAnyRect(sepCtx.boxTopBands, x1, y1, x2, y2, 1)) return false;
    return true;
  }

  function segOverlapsAxisCoord(p1, p2, isH, newCoord, segMin, segMax) {
    if (isH) {
      if (Math.abs(p1.y - p2.y) >= 0.5 || Math.abs(newCoord - p1.y) >= 5) return false;
      return Math.min(segMax, Math.max(p1.x, p2.x)) > Math.max(segMin, Math.min(p1.x, p2.x)) + 1;
    }
    if (Math.abs(p1.x - p2.x) >= 0.5 || Math.abs(newCoord - p1.x) >= 5) return false;
    return Math.min(segMax, Math.max(p1.y, p2.y)) > Math.max(segMin, Math.min(p1.y, p2.y)) + 1;
  }

  function pathSegmentConflicts(opts, skipOwnIdx, isH, newCoord, segMin, segMax) {
    for (let osi = 0; osi < opts.length - 1; osi++) {
      if (skipOwnIdx >= 0 && Math.abs(osi - skipOwnIdx) <= 1) continue;
      if (segOverlapsAxisCoord(opts[osi], opts[osi + 1], isH, newCoord, segMin, segMax)) return true;
    }
    return false;
  }

  function conflictsWithOtherPaths(sepCtx, ei, si, isH, newCoord) {
    const { allPaths, routeMetaList } = sepCtx;
    const { bundleId, isTrunk } = routeMetaList[ei];
    const a = allPaths[ei][si];
    const b = allPaths[ei][si + 1];
    const segMin = isH ? Math.min(a.x, b.x) : Math.min(a.y, b.y);
    const segMax = isH ? Math.max(a.x, b.x) : Math.max(a.y, b.y);
    for (let oei = 0; oei < allPaths.length; oei++) {
      const ometa = routeMetaList[oei];
      if (bundleId && ometa.bundleId === bundleId && !isTrunk && !ometa.isTrunk) continue;
      const skipOwnIdx = oei === ei ? si : -1;
      if (pathSegmentConflicts(allPaths[oei], skipOwnIdx, isH, newCoord, segMin, segMax)) return true;
    }
    return false;
  }

  function canShiftSegment(sepCtx, ei, si, isH, newCoord) {
    const limit = isH ? sepCtx.stageH : sepCtx.stageW;
    if (newCoord < 6 || newCoord > limit - 6) return false;
    const pts = sepCtx.allPaths[ei];
    const meta = sepCtx.routeMetaList[ei];
    const a = pts[si];
    const b = pts[si + 1];
    const prev = pts[si - 1];
    const next = pts[si + 2];
    const clear = isH
      ? isShiftedSegClear(sepCtx, meta, a.x, newCoord, b.x, newCoord) &&
        isShiftedSegClear(sepCtx, meta, prev.x, prev.y, a.x, newCoord) &&
        isShiftedSegClear(sepCtx, meta, b.x, newCoord, next.x, next.y)
      : isShiftedSegClear(sepCtx, meta, newCoord, a.y, newCoord, b.y) &&
        isShiftedSegClear(sepCtx, meta, prev.x, prev.y, newCoord, a.y) &&
        isShiftedSegClear(sepCtx, meta, newCoord, b.y, next.x, next.y);
    if (!clear) return false;
    return !conflictsWithOtherPaths(sepCtx, ei, si, isH, newCoord);
  }

  function assignBucketLanes(list) {
    // Group same-bundle taps into one logical lane so collector/distributor spines stay merged.
    const groupsMap = new Map();
    for (const item of list) {
      let g = groupsMap.get(item.laneGroup);
      if (!g) {
        g = { laneGroup: item.laneGroup, min: item.min, max: item.max, items: [] };
        groupsMap.set(item.laneGroup, g);
      }
      g.min = Math.min(g.min, item.min);
      g.max = Math.max(g.max, item.max);
      g.items.push(item);
    }
    const groups = Array.from(groupsMap.values());
    groups.sort((u, v) => u.min - v.min);
    const lanes = [];
    for (const g of groups) {
      let lane = 0;
      while (lane < lanes.length && lanes[lane] > g.min + 2) lane++;
      lanes[lane] = g.max;
      g.lane = lane;
    }
    return { groups, totalLanes: lanes.length };
  }

  function tryShiftBucketItem(sepCtx, item, isH, offset) {
    const pts = sepCtx.allPaths[item.ei];
    const a = pts[item.si];
    const b = pts[item.si + 1];
    const base = isH ? a.y : a.x;
    for (const mult of [1, 0.5]) {
      const candidate = base + offset * mult;
      if (!canShiftSegment(sepCtx, item.ei, item.si, isH, candidate)) continue;
      if (isH) {
        a.y = candidate;
        b.y = candidate;
      } else {
        a.x = candidate;
        b.x = candidate;
      }
      return;
    }
  }

  function shiftLaneGroup(sepCtx, group, offset, isH) {
    for (const item of group.items) {
      tryShiftBucketItem(sepCtx, item, isH, offset);
    }
  }

  function applyParallelBucketOffsets(sepCtx, buckets, isH) {
    for (const list of buckets.values()) {
      if (list.length <= 1) continue;
      const { groups, totalLanes } = assignBucketLanes(list);
      if (totalLanes <= 1) continue;
      for (const g of groups) {
        const offset = (g.lane - (totalLanes - 1) / 2) * LANE_STEP;
        if (Math.abs(offset) < 0.5) continue;
        shiftLaneGroup(sepCtx, g, offset, isH);
      }
    }
  }

  function separateParallelSegments(allPaths, routeMetaList, viewCtx) {
    const { hBuckets, vBuckets } = collectParallelBuckets(allPaths, routeMetaList);
    const sepCtx = {
      allPaths,
      routeMetaList,
      cardRects: viewCtx.cardRects,
      headerRects: viewCtx.headerRects,
      boxTopBands: viewCtx.boxTopBands,
      stageW: viewCtx.stageW,
      stageH: viewCtx.stageH
    };
    applyParallelBucketOffsets(sepCtx, hBuckets, true);
    applyParallelBucketOffsets(sepCtx, vBuckets, false);
  }

  function collectHorizontalCrosses(a, b, verticalSegs, routeIdx, bundleId) {
    const minX = Math.min(a.x, b.x);
    const maxX = Math.max(a.x, b.x);
    const crosses = [];
    for (const vs of verticalSegs) {
      if (vs.routeIdx === routeIdx) continue;
      if (bundleId && vs.bundleId === bundleId) continue;
      if (vs.x1 > minX + 7 && vs.x1 < maxX - 7 && a.y > vs.minY + 4 && a.y < vs.maxY - 4) {
        crosses.push(vs.x1);
      }
    }
    return crosses;
  }

  function buildHorizontalJumperPath(a, b, verticalSegs, routeIdx, bundleId) {
    const crosses = collectHorizontalCrosses(a, b, verticalSegs, routeIdx, bundleId);
    if (crosses.length === 0) return null;

    const dir = b.x >= a.x ? 1 : -1;
    crosses.sort((u, v) => (dir > 0 ? u - v : v - u));
    let d = `M ${a.x.toFixed(1)} ${a.y.toFixed(1)}`;
    let prevX = a.x;
    let jumped = false;
    for (const cx of crosses) {
      const r = BRIDGE_RADIUS;
      if (Math.abs(cx - prevX) < r * 2.2) continue;
      const xBefore = cx - dir * r;
      const xAfter = cx + dir * r;
      const sweep = dir > 0 ? 1 : 0;
      d += ` L ${xBefore.toFixed(1)} ${a.y.toFixed(1)} A ${r} ${r} 0 0 ${sweep} ${xAfter.toFixed(1)} ${a.y.toFixed(1)}`;
      prevX = xAfter;
      jumped = true;
    }
    if (!jumped) return null;
    d += ` L ${b.x.toFixed(1)} ${b.y.toFixed(1)}`;
    return d;
  }

  function measureSheetAR(sheet) {
    const r = sheet.getBoundingClientRect();
    return { w: r.width, h: r.height, ar: r.width / Math.max(1, r.height) };
  }

  function shouldDockGovBottom(m, stage, gov) {
    const govItems = gov.querySelectorAll('.gov-item').length;
    const sr = stage.getBoundingClientRect();
    const gr = gov.getBoundingClientRect();
    return m.ar > 2.35 || (govItems >= 12 && gr.height > sr.height * 1.15) || gr.height > sr.height * 1.45;
  }

  function narrowWideStage(sheet, stage) {
    const candidates = Array.from(stage.querySelectorAll('.arch-box-body'))
      .filter(el => !el.classList.contains('stack-vertical') && el.children.length > 1)
      .map(el => ({ el, width: el.getBoundingClientRect().width }));
    candidates.sort((a, b) => b.width - a.width);
    for (const { el: cand } of candidates) {
      cand.classList.add('stack-vertical');
      const m = measureSheetAR(sheet);
      if (m.ar <= 2.35) return m;
    }
    const sr = stage.getBoundingClientRect();
    stage.classList.add('wrap-stage');
    stage.style.maxWidth = Math.max(680, Math.ceil(sr.width * 0.56)) + 'px';
    return measureSheetAR(sheet);
  }

  function widenTallSheet(sheet, viewBody, stage, gov, m) {
    if (gov && !viewBody.classList.contains('dock-bottom')) {
      const sr = stage.getBoundingClientRect();
      const gr = gov.getBoundingClientRect();
      if ((gr.height > sr.height * 1.25 && sr.width < 1500) || m.ar < 0.42) {
        viewBody.classList.add('wide-gov');
        m = measureSheetAR(sheet);
      }
    }
    if (m.ar < 0.42) {
      for (const st of stage.querySelectorAll('.arch-box-body.stack-vertical')) {
        st.classList.remove('stack-vertical');
        m = measureSheetAR(sheet);
        if (m.ar >= 0.42) break;
      }
    }
  }

  function balanceViewAspectRatio(sheet) {
    const viewBody = sheet.querySelector('.view-body');
    const stage = sheet.querySelector('.diagram-stage');
    const gov = sheet.querySelector('.governance-panel');
    if (!viewBody || !stage) return;

    let m = measureSheetAR(sheet);
    if (gov && !viewBody.classList.contains('dock-bottom') && shouldDockGovBottom(m, stage, gov)) {
      viewBody.classList.remove('wide-gov');
      viewBody.classList.add('dock-bottom');
      m = measureSheetAR(sheet);
    }
    if (m.ar > 2.35) {
      m = narrowWideStage(sheet, stage);
    }
    widenTallSheet(sheet, viewBody, stage, gov, m);
  }

  function applyCorridorSpacing(sheet, pass, expandBorders) {
    if (pass <= 0) return;
    const stage = sheet.querySelector('.diagram-stage');
    const stageGap = pass === 1 ? 96 : Math.min(208, 80 + pass * 42);
    const bodyGap = Math.min(150, 60 + pass * 24);
    const laneGap = Math.min(64, 30 + pass * 10);
    if (stage) {
      stage.style.columnGap = stageGap + 'px';
      stage.style.rowGap = Math.max(48, laneGap + 18) + 'px';
    }
    sheet.querySelectorAll('.arch-box-body:not(.stack-vertical)').forEach(el => {
      el.style.gap = bodyGap + 'px';
    });
    sheet.querySelectorAll('.projects-row').forEach(el => {
      el.style.gap = bodyGap + 'px';
    });
    sheet.querySelectorAll('.node-lane').forEach(el => {
      el.style.gap = laneGap + 'px';
    });
    if (expandBorders) {
      const boxPadBottom = Math.min(48, 24 + pass * 10);
      const boxRowGap = Math.min(34, 16 + pass * 6);
      const stackGap = Math.min(54, 34 + pass * 8);
      sheet.querySelectorAll('.arch-box').forEach(el => {
        el.style.paddingBottom = boxPadBottom + 'px';
        el.style.rowGap = boxRowGap + 'px';
      });
      sheet.querySelectorAll('.arch-box-body.stack-vertical, .vpc-regions-stack, .projects-stack').forEach(el => {
        el.style.gap = stackGap + 'px';
      });
    }
    sheet.getBoundingClientRect();
  }

  function collectBoxAncestors(el, stage) {
    const set = new Set();
    let cur = el.closest('.arch-box');
    while (cur && stage.contains(cur)) {
      const cid = cur.dataset.containerId || '';
      if (cid) set.add(cid);
      cur = cur.parentElement ? cur.parentElement.closest('.arch-box') : null;
    }
    return set;
  }

  function buildBusBundles(activeEdges) {
    const used = new Set();
    const bundles = [];

    const dominantVerb = (idxs) => {
      const counts = new Map();
      for (const idx of idxs) {
        const v = (activeEdges[idx].verb || '').trim();
        if (!v) continue;
        counts.set(v, (counts.get(v) || 0) + 1);
      }
      let best = '';
      let bestC = -1;
      for (const [v, c] of counts.entries()) {
        if (c > bestC || (c === bestC && v < best)) {
          best = v;
          bestC = c;
        }
      }
      return best;
    };

    const collectBundles = (indices, minSize, kind, keyFn, labelFn) => {
      const groups = new Map();
      for (const idx of indices) {
        if (used.has(idx)) continue;
        const k = keyFn(activeEdges[idx]);
        if (!groups.has(k)) groups.set(k, []);
        groups.get(k).push(idx);
      }
      const isFanIn = kind === 'fan_in';
      for (const idxs of groups.values()) {
        if (idxs.length < minSize) continue;
        idxs.forEach(i => used.add(i));
        const sample = activeEdges[idxs[0]];
        bundles.push({
          id: `b_${bundles.length}`,
          kind,
          hubId: isFanIn ? sample.to : sample.from,
          memberIds: Array.from(new Set(idxs.map(i => (isFanIn ? activeEdges[i].from : activeEdges[i].to)))),
          edgeIndices: idxs,
          label: labelFn(idxs, sample),
          edgeSample: sample
        });
      }
    };

    const allIdxs = activeEdges
      .map((_, i) => i)
      .filter(i => (activeEdges[i].verb || '').trim() !== '' && activeEdges[i].from !== activeEdges[i].to);

    const exactVerbLabel = (idxs, sample) => `${(sample.verb || '').trim()} (${idxs.length}×)`;
    collectBundles(allIdxs, 2, 'fan_in', e => `${e.to}|${(e.verb || '').trim()}|${edgeStyle(e).tag}`, exactVerbLabel);
    collectBundles(allIdxs, 2, 'fan_out', e => `${e.from}|${(e.verb || '').trim()}|${edgeStyle(e).tag}`, exactVerbLabel);
    collectBundles(allIdxs, 3, 'fan_in', e => `${e.to}|${edgeStyle(e).tag}`, idxs => `${dominantVerb(idxs)} (${idxs.length}×)`);

    const singletons = activeEdges.map((_, i) => i).filter(i => !used.has(i));
    return { bundles, singletons };
  }

  function pointInsideRect(x, y, rect, pad) {
    return x >= rect.left - pad && x <= rect.right + pad && y >= rect.top - pad && y <= rect.bottom + pad;
  }

  function pointHitsAnyRect(x, y, rects, pad) {
    for (const r of rects) {
      if (pointInsideRect(x, y, r, pad)) return true;
    }
    return false;
  }

  function isJunctionPointClear(x, y, viewCtx) {
    return !pointHitsAnyRect(x, y, viewCtx.cardRects, 10) &&
      !pointHitsAnyRect(x, y, viewCtx.headerRects, 8) &&
      !pointHitsAnyRect(x, y, viewCtx.boxTopBands, 4) &&
      !pointHitsAnyRect(x, y, viewCtx.borderBands, 6);
  }

  function commonAncestorBoxIds(nodeIds, ancestorsOfNode) {
    const firstAnc = ancestorsOfNode.get(nodeIds[0]) || new Set();
    const common = new Set();
    for (const cid of firstAnc) {
      if (nodeIds.every(nid => (ancestorsOfNode.get(nid) || new Set()).has(cid))) {
        common.add(cid);
      }
    }
    return common;
  }

  function scoreJunctionCandidate(x, y, hubRect, memberRects, nonCommonBoxes, usedJunctions) {
    let score = 0;
    for (const uj of usedJunctions) {
      if (Math.hypot(x - uj.x, y - uj.y) < 16) {
        score += 2000;
        break;
      }
    }
    for (const ncb of nonCommonBoxes) {
      if (pointInsideRect(x, y, ncb, 2)) score += 5000;
    }
    let tapSum = 0;
    for (const mr of memberRects) {
      tapSum += Math.abs(x - mr.cx) + Math.abs(y - mr.cy);
    }
    const avgTap = tapSum / Math.max(1, memberRects.length);
    const trunkDist = Math.abs(x - hubRect.cx) + Math.abs(y - hubRect.cy);
    const shortTrunkPen = trunkDist < 95 ? (95 - trunkDist) * 5 : 0;
    return score + avgTap * 0.7 + trunkDist * 0.5 + shortTrunkPen;
  }

  function fallbackJunctionPoint(xs, ys, hubRect) {
    return {
      x: xs[Math.floor(xs.length / 2)] || hubRect.left - 24,
      y: ys[Math.floor(ys.length / 2)] || hubRect.cy
    };
  }

  function pickBundleJunction(bundle, viewCtx, usedJunctions) {
    const { xs, ys, nodeRects, boxEntries, ancestorsOfNode } = viewCtx;
    const hubRect = nodeRects.get(bundle.hubId);
    const memberRects = bundle.memberIds.map(id => nodeRects.get(id)).filter(Boolean);
    const commonAnc = commonAncestorBoxIds([bundle.hubId, ...bundle.memberIds], ancestorsOfNode);
    const nonCommonBoxes = boxEntries.filter(b => b.id && !commonAnc.has(b.id)).map(b => b.stageRect);

    let bestPt = null;
    let bestScore = Infinity;
    for (const x of xs) {
      for (const y of ys) {
        if (!isJunctionPointClear(x, y, viewCtx)) continue;
        const score = scoreJunctionCandidate(x, y, hubRect, memberRects, nonCommonBoxes, usedJunctions);
        if (score < bestScore) {
          bestScore = score;
          bestPt = { x, y };
        }
      }
    }

    bestPt = bestPt || fallbackJunctionPoint(xs, ys, hubRect);
    usedJunctions.push(bestPt);
    return bestPt;
  }

  function headerContentRect(hdr, origin) {
    const hrFull = relRect(hdr, origin);
    let kidsLeft = Infinity;
    let kidsRight = -Infinity;
    for (const child of hdr.children) {
      const cr = relRect(child, origin);
      if (cr.width <= 0 && cr.height <= 0) continue;
      if (cr.left < kidsLeft) kidsLeft = cr.left;
      if (cr.right > kidsRight) kidsRight = cr.right;
    }
    if (!Number.isFinite(kidsLeft) || !Number.isFinite(kidsRight) || kidsRight <= kidsLeft) {
      return hrFull;
    }
    return {
      left: kidsLeft,
      top: hrFull.top,
      right: kidsRight,
      bottom: hrFull.bottom,
      width: kidsRight - kidsLeft,
      height: hrFull.height,
      cx: (kidsLeft + kidsRight) / 2,
      cy: hrFull.cy
    };
  }

  function collectClientRectLines(rects) {
    const lines = [];
    for (const r of rects) {
      if (r.width < 0.5 || r.height < 0.5) continue;
      if (lines.length === 0 || Math.abs(r.top - lines.at(-1).top) > 2) {
        lines.push({ top: r.top });
      }
    }
    return lines;
  }

  function countTrailingLineChars(tnode, start, end, lastTop) {
    let trailingChars = 0;
    for (let ci = end - 1; ci >= start; ci--) {
      const cr = document.createRange();
      cr.setStart(tnode, ci);
      cr.setEnd(tnode, ci + 1);
      if (Math.abs(cr.getBoundingClientRect().top - lastTop) > 2) break;
      trailingChars++;
    }
    return trailingChars;
  }

  function auditTitleTextNode(tnode, viewId, overflowTexts, collisions) {
    const text = tnode.nodeValue || '';
    const tokenRegex = /\S+/g;
    let match;
    while ((match = tokenRegex.exec(text)) !== null) {
      if (match[0].length < 4) continue;
      const start = match.index;
      const end = start + match[0].length;
      const range = document.createRange();
      range.setStart(tnode, start);
      range.setEnd(tnode, end);
      const lines = collectClientRectLines(Array.from(range.getClientRects()));
      if (lines.length < 2) continue;
      const trailingChars = countTrailingLineChars(tnode, start, end, lines.at(-1).top);
      if (trailingChars > 0 && trailingChars <= 3) {
        const msg = `text_orphan_wrap: ${match[0]} splits with ${trailingChars} trailing chars`;
        overflowTexts.push(msg);
        collisions.push({ view_id: viewId, kind: 'text_orphan_wrap', subjects: [match[0].slice(0, 40)], detail: msg });
      }
    }
  }

  function auditSheetText(sheet, viewId) {
    const clippedTexts = [];
    const overflowTexts = [];
    const collisions = [];

    for (const el of sheet.querySelectorAll(TEXT_AUDIT_SELECTOR)) {
      const cs = getComputedStyle(el);
      const txt = (el.textContent || '').trim();
      if (cs.textOverflow === 'ellipsis' || txt.endsWith('...') || txt.endsWith('…')) {
        const msg = `${el.className}: ellipsis on ${txt.slice(0, 40)}`;
        clippedTexts.push(msg);
        collisions.push({ view_id: viewId, kind: 'text_ellipsis', subjects: [txt.slice(0, 40)], detail: msg });
      }
      if (el.scrollWidth > el.clientWidth + 1 || el.scrollHeight > el.clientHeight + 1) {
        const msg = `${el.className}: overflow (${el.scrollWidth}x${el.scrollHeight} > ${el.clientWidth}x${el.clientHeight}) on ${txt.slice(0, 40)}`;
        overflowTexts.push(msg);
        collisions.push({ view_id: viewId, kind: 'text_overflow', subjects: [txt.slice(0, 40)], detail: msg });
      }
    }

    for (const titleEl of sheet.querySelectorAll('.arch-card-title')) {
      const walker = document.createTreeWalker(titleEl, NodeFilter.SHOW_TEXT);
      let tnode;
      while ((tnode = walker.nextNode())) {
        auditTitleTextNode(tnode, viewId, overflowTexts, collisions);
      }
    }

    return { clippedTexts, overflowTexts, collisions };
  }

  function build2DShifts(dxs, dys) {
    const pairs = [];
    for (const dx of dxs) {
      for (const dy of dys) {
        pairs.push([dx, dy]);
      }
    }
    pairs.sort((u, v) => (Math.abs(u[0]) + Math.abs(u[1])) - (Math.abs(v[0]) + Math.abs(v[1])));
    return pairs;
  }

  const PILL_SHIFTS_2D_H = build2DShifts([0, -16, 16, -32, 32, -48, 48, -64, 64], [0, -12, 12, -22, 22, -32, 32, -44, 44]);
  const PILL_SHIFTS_2D_V = build2DShifts([0, -16, 16, -28, 28, -42, 42, -56, 56, -72, 72], [0, -10, 10, -20, 20, -32, 32, -46, 46]);
  const PILL_SHIFTS_1D_H = [[0, 0], [0, -13], [0, 13], [0, -22], [0, 22], [0, -32], [0, 32]];
  const PILL_SHIFTS_1D_V = [[0, 0], [-18, 0], [18, 0], [-30, 0], [30, 0], [-42, 0], [42, 0], [-54, 0], [54, 0]];
  const PILL_TS_STRICT = [0.5, 0.38, 0.62, 0.28, 0.72, 0.18, 0.82, 0.12, 0.88];
  const PILL_TS_RELAXED = [0.5, 0.38, 0.62, 0.28, 0.72, 0.18, 0.82, 0.12, 0.88, 0.06, 0.94, 0.44, 0.56, 0.23, 0.77];
  const PILL_PASSES = [
    { strictBoxes: true, borderPad: 5.5, segPad: 6, arrowPad: 3, pillPad: 4, cardPad: 4, headerPad: 3, use2D: false },
    { strictBoxes: false, borderPad: 5.5, segPad: 6, arrowPad: 3, pillPad: 4, cardPad: 4, headerPad: 3, use2D: false },
    { strictBoxes: true, borderPad: 4.5, segPad: 3.5, arrowPad: 2, pillPad: 2, cardPad: 2, headerPad: 2, use2D: true },
    { strictBoxes: false, borderPad: 4.5, segPad: 3.5, arrowPad: 2, pillPad: 2, cardPad: 2, headerPad: 2, use2D: true }
  ];

  function pillShiftsFor(use2D, isH) {
    if (use2D) return isH ? PILL_SHIFTS_2D_H : PILL_SHIFTS_2D_V;
    return isH ? PILL_SHIFTS_1D_H : PILL_SHIFTS_1D_V;
  }

  function measureStageGeometry(stage, stageRect) {
    const ancestorsOfNode = new Map();
    const nodeRects = new Map();
    const cardEntries = [];
    const cardRects = [];
    for (const card of stage.querySelectorAll('.arch-card')) {
      const r = relRect(card, stageRect);
      const id = card.dataset.componentId || '';
      if (id) {
        nodeRects.set(id, r);
        ancestorsOfNode.set(id, collectBoxAncestors(card, stage));
      }
      cardRects.push(r);
      cardEntries.push({ id, stageRect: r });
    }

    const boxEntries = [];
    const boxRects = [];
    const boxTopBands = [];
    const headerEntries = [];
    const headerRects = [];
    const borderBands = [];
    for (const box of stage.querySelectorAll('.arch-box')) {
      const br = relRect(box, stageRect);
      const cid = box.dataset.containerId || '';
      boxRects.push(br);
      if (box.dataset.anchorId) {
        nodeRects.set(box.dataset.anchorId, br);
        ancestorsOfNode.set(box.dataset.anchorId, collectBoxAncestors(box, stage));
      }
      const hdr = box.querySelector(':scope > .arch-box-header');
      let hr = null;
      if (hdr) {
        hr = headerContentRect(hdr, stageRect);
        headerRects.push(hr);
        headerEntries.push({ id: cid + ':header', stageRect: hr });
      }
      const topBand = { left: br.left, top: br.top, right: br.right, bottom: hr ? hr.bottom : br.top + 14 };
      boxTopBands.push(topBand);
      boxEntries.push({ id: cid, stageRect: br, topBand });
      borderBands.push(
        { left: br.left - 2, top: br.top - 2, right: br.right + 2, bottom: br.top + 2 },
        { left: br.left - 2, top: br.bottom - 2, right: br.right + 2, bottom: br.bottom + 2 },
        { left: br.left - 2, top: br.top - 2, right: br.left + 2, bottom: br.bottom + 2 },
        { left: br.right - 2, top: br.top - 2, right: br.right + 2, bottom: br.bottom + 2 }
      );
    }

    return {
      ancestorsOfNode,
      nodeRects,
      cardEntries,
      cardRects,
      boxEntries,
      boxRects,
      boxTopBands,
      headerEntries,
      headerRects,
      borderBands
    };
  }

  function makeSingletonRouteItem(e, ei) {
    return {
      from: e.from,
      to: e.to,
      srcPoint: null,
      dstPoint: null,
      is_retry_or_dlq: e.is_retry_or_dlq,
      edgeSample: e,
      isTrunk: false,
      bundleId: null,
      edgeIdx: ei,
      hasArrow: true,
      allowedNodeIds: [e.from, e.to],
      label: `${e.from}->${e.to}`
    };
  }

  function addBundleTrunk(b, viewCtx, usedJunctions, plan) {
    b.junction = pickBundleJunction(b, viewCtx, usedJunctions);
    const st = edgeStyle(b.edgeSample);
    plan.junctions.push({ x: b.junction.x, y: b.junction.y, color: st.color });
    const isFanIn = b.kind === 'fan_in';
    plan.bundleAllRouteIdxs.set(b.id, [plan.routeItems.length]);
    plan.routeItems.push({
      from: isFanIn ? '' : b.hubId,
      to: isFanIn ? b.hubId : '',
      srcPoint: isFanIn ? b.junction : null,
      dstPoint: isFanIn ? null : b.junction,
      is_retry_or_dlq: b.edgeSample.is_retry_or_dlq,
      edgeSample: b.edgeSample,
      isTrunk: true,
      bundleId: b.id,
      bundle: b,
      hasArrow: isFanIn,
      allowedNodeIds: [b.hubId],
      label: `${b.id}:${b.label}`
    });
  }

  function addBundleTaps(b, plan) {
    const isFanIn = b.kind === 'fan_in';
    const idxs = plan.bundleAllRouteIdxs.get(b.id);
    for (const memberId of b.memberIds) {
      idxs.push(plan.routeItems.length);
      plan.routeItems.push({
        from: isFanIn ? memberId : '',
        to: isFanIn ? '' : memberId,
        srcPoint: isFanIn ? null : b.junction,
        dstPoint: isFanIn ? b.junction : null,
        is_retry_or_dlq: b.edgeSample.is_retry_or_dlq,
        edgeSample: b.edgeSample,
        isTrunk: false,
        bundleId: b.id,
        hasArrow: !isFanIn,
        allowedNodeIds: [memberId],
        label: `${b.id}:${memberId}`
      });
    }
  }

  function buildRoutePlan(activeEdges, viewCtx) {
    const plan = {
      routeItems: [],
      junctions: [],
      bundleAllRouteIdxs: new Map(),
      singletonRouteIdxByEdge: new Map()
    };
    if (activeEdges.length < 20) {
      for (let ei = 0; ei < activeEdges.length; ei++) {
        plan.singletonRouteIdxByEdge.set(ei, ei);
        plan.routeItems.push(makeSingletonRouteItem(activeEdges[ei], ei));
      }
      return plan;
    }

    const { bundles, singletons } = buildBusBundles(activeEdges);
    const usedJunctions = [];
    for (const b of bundles) {
      addBundleTrunk(b, viewCtx, usedJunctions, plan);
    }
    for (const ei of singletons) {
      plan.singletonRouteIdxByEdge.set(ei, plan.routeItems.length);
      plan.routeItems.push(makeSingletonRouteItem(activeEdges[ei], ei));
    }
    for (const b of bundles) {
      addBundleTaps(b, plan);
    }
    return plan;
  }

  function collectAllowedBoxes(nodeIds, ancestorsOfNode) {
    const allowed = new Set();
    for (const nid of nodeIds) {
      const set = ancestorsOfNode.get(nid);
      if (!set) continue;
      for (const cid of set) allowed.add(cid);
    }
    return allowed;
  }

  function buildFinalSegments(routeItems, routedPaths) {
    const finalSegs = [];
    const verticalSegs = [];
    const segsByRouteIdx = Array.from({ length: routeItems.length }, () => []);
    const endpointArrowRects = [];
    for (let i = 0; i < routedPaths.length; i++) {
      const pts = routedPaths[i];
      const item = routeItems[i];
      if (item.hasArrow && pts.length >= 2) {
        const pEnd = pts.at(-1);
        endpointArrowRects.push({
          routeIdx: i,
          left: pEnd.x - 5,
          top: pEnd.y - 5,
          right: pEnd.x + 5,
          bottom: pEnd.y + 5
        });
      }
      for (let j = 0; j + 1 < pts.length; j++) {
        const a = pts[j];
        const b = pts[j + 1];
        const isH = Math.abs(a.y - b.y) < 0.5;
        const seg = {
          routeIdx: i,
          bundleId: item.bundleId,
          isTrunk: item.isTrunk,
          hasArrow: item.hasArrow,
          edgeSample: item.edgeSample,
          segIdx: j,
          totalSegs: pts.length - 1,
          isH,
          x1: a.x, y1: a.y, x2: b.x, y2: b.y,
          minX: Math.min(a.x, b.x), maxX: Math.max(a.x, b.x),
          minY: Math.min(a.y, b.y), maxY: Math.max(a.y, b.y)
        };
        finalSegs.push(seg);
        segsByRouteIdx[i].push(seg);
        if (!isH) verticalSegs.push(seg);
      }
    }
    return { finalSegs, verticalSegs, segsByRouteIdx, endpointArrowRects };
  }

  function routeAllItems(routeItems, viewCtx, workspace) {
    const { starts, ends } = computePorts(routeItems, viewCtx.nodeRects);
    const routedPaths = [];
    const routedSegs = [];
    const routeMetaList = [];
    const routeCtx = {
      gridMasks: viewCtx.gridMasks,
      cardRects: viewCtx.cardRects,
      headerRects: viewCtx.headerRects,
      boxTopBands: viewCtx.boxTopBands,
      routedSegs,
      stageW: viewCtx.stageW,
      stageH: viewCtx.stageH,
      workspace
    };

    for (let i = 0; i < routeItems.length; i++) {
      const item = routeItems[i];
      const srcRect = item.srcPoint ? null : viewCtx.nodeRects.get(item.from);
      const dstRect = item.dstPoint ? null : viewCtx.nodeRects.get(item.to);
      const allowedBoxes = collectAllowedBoxes(item.allowedNodeIds, viewCtx.ancestorsOfNode);
      const nonAncestorBoxes = viewCtx.boxEntries
        .filter(b => b.id && !allowedBoxes.has(b.id))
        .map(b => b.stageRect);
      const edgeCtx = {
        srcRect,
        dstRect,
        nonAncestorBoxes,
        allowedBoxes,
        bundleId: item.bundleId,
        isTrunk: item.isTrunk
      };
      routeMetaList.push({
        ...edgeCtx,
        hasArrow: item.hasArrow,
        label: item.label
      });

      const pts = routeOrthogonalAStar(starts[i], ends[i], routeCtx, edgeCtx);
      routedPaths.push(pts);
      for (let j = 0; j + 1 < pts.length; j++) {
        const a = pts[j];
        const b = pts[j + 1];
        routedSegs.push({
          routeIdx: i,
          bundleId: item.bundleId,
          isTrunk: item.isTrunk,
          isH: Math.abs(a.y - b.y) < 0.5,
          x1: a.x, y1: a.y, x2: b.x, y2: b.y,
          minX: Math.min(a.x, b.x), maxX: Math.max(a.x, b.x),
          minY: Math.min(a.y, b.y), maxY: Math.max(a.y, b.y)
        });
      }
    }

    separateParallelSegments(routedPaths, routeMetaList, viewCtx);
    return { routeMetaList, ...buildFinalSegments(routeItems, routedPaths) };
  }

  function overlapsAnyRect(pr, rects, pad) {
    for (const r of rects) {
      if (rectsOverlap(pr, r, pad)) return true;
    }
    return false;
  }

  function overlapsAnyJunction(pr, junctions) {
    for (const jpt of junctions) {
      const jr = { left: jpt.x - 4, top: jpt.y - 4, right: jpt.x + 4, bottom: jpt.y + 4 };
      if (rectsOverlap(pr, jr, 4)) return true;
    }
    return false;
  }

  function hitsOtherFinalSeg(pr, finalSegs, owningSeg, segPad) {
    for (const s of finalSegs) {
      if (s !== owningSeg && segIntersectsRect(s.x1, s.y1, s.x2, s.y2, pr, segPad)) return true;
    }
    return false;
  }

  function isPillClear(pillCtx, pr, owningSeg, nonAncestorBoxes, opts) {
    const {
      strictBoxes = true,
      borderPad = 5.5,
      segPad = 6,
      arrowPad = 3,
      pillPad = 4,
      cardPad = 4,
      headerPad = 3
    } = opts;
    if (pr.left < 4 || pr.top < 4 || pr.right > pillCtx.stageW - 4 || pr.bottom > pillCtx.stageH - 4) return false;
    if (overlapsAnyRect(pr, pillCtx.cardRects, cardPad)) return false;
    if (overlapsAnyRect(pr, pillCtx.headerRects, headerPad)) return false;
    if (overlapsAnyRect(pr, pillCtx.borderBands, borderPad)) return false;
    if (strictBoxes && (overlapsAnyRect(pr, nonAncestorBoxes, 2) || overlapsAnyRect(pr, pillCtx.boxTopBands, 1))) {
      return false;
    }
    if (overlapsAnyJunction(pr, pillCtx.junctions)) return false;
    if (overlapsAnyRect(pr, pillCtx.endpointArrowRects, arrowPad)) return false;
    if (pillCtx.placedPills.some(pp => rectsOverlap(pr, pp.rect, pillPad))) return false;
    return !hitsOtherFinalSeg(pr, pillCtx.finalSegs, owningSeg, segPad);
  }

  function endpointPreferenceRank(seg, preferEnd) {
    if (preferEnd === 'dst') return seg.j === seg.total - 1 ? 1 : 0;
    if (preferEnd === 'src') return seg.j === 0 ? 1 : 0;
    return 0;
  }

  function comparePillSegments(u, v, pw, preferEnd) {
    const uRank = endpointPreferenceRank(u, preferEnd);
    const vRank = endpointPreferenceRank(v, preferEnd);
    if (uRank !== vRank) return vRank - uRank;
    const uHorizFit = u.isH && u.len >= pw + 18;
    const vHorizFit = v.isH && v.len >= pw + 18;
    if (uHorizFit !== vHorizFit) return uHorizFit ? -1 : 1;
    return v.len - u.len;
  }

  function collectCandidatePillSegs(pillCtx, routeIndices) {
    const allowedBoxUnion = new Set();
    const segs = [];
    for (const ri of routeIndices) {
      const meta = pillCtx.routeMetaList[ri];
      if (meta) {
        for (const cid of meta.allowedBoxes) allowedBoxUnion.add(cid);
      }
      for (const seg of pillCtx.segsByRouteIdx[ri]) {
        const len = Math.hypot(seg.x2 - seg.x1, seg.y2 - seg.y1);
        if (len >= 12) {
          segs.push({ a: { x: seg.x1, y: seg.y1 }, b: { x: seg.x2, y: seg.y2 }, len, isH: seg.isH, j: seg.segIdx, total: seg.totalSegs, segRef: seg });
        }
      }
    }
    const nonAncestorBoxes = pillCtx.boxEntries
      .filter(b => b.id && !allowedBoxUnion.has(b.id))
      .map(b => b.stageRect);
    return { segs, nonAncestorBoxes };
  }

  function tryPlacePillOnSegment(pillCtx, sg, spec, nonAncestorBoxes, passOpts) {
    const ts = passOpts.use2D ? PILL_TS_RELAXED : PILL_TS_STRICT;
    const shifts = pillShiftsFor(passOpts.use2D, sg.isH);
    const { pw, ph, label, color, edgeIndices } = spec;
    for (const t of ts) {
      const ax = sg.a.x + (sg.b.x - sg.a.x) * t;
      const ay = sg.a.y + (sg.b.y - sg.a.y) * t;
      for (const [dx, dy] of shifts) {
        const cx = ax + dx;
        const cy = ay + dy;
        const pr = {
          left: cx - pw / 2,
          top: cy - ph / 2,
          right: cx + pw / 2,
          bottom: cy + ph / 2,
          width: pw,
          height: ph
        };
        if (isPillClear(pillCtx, pr, sg.segRef, nonAncestorBoxes, passOpts)) {
          return { rect: pr, cx, cy, label, color, edgeIndices, owningSeg: sg.segRef };
        }
      }
    }
    return null;
  }

  function tryPlacePill(pillCtx, label, routeIndices, edgeIndices, color, preferEnd) {
    const pw = measurePillWidth(label, pillCtx.pillFontSize);
    const ph = pillCtx.pillHeight;
    const { segs, nonAncestorBoxes } = collectCandidatePillSegs(pillCtx, routeIndices);
    segs.sort((u, v) => comparePillSegments(u, v, pw, preferEnd));
    const spec = { label, color, edgeIndices, pw, ph };
    for (const passOpts of PILL_PASSES) {
      for (const sg of segs) {
        const placed = tryPlacePillOnSegment(pillCtx, sg, spec, nonAncestorBoxes, passOpts);
        if (placed) return placed;
      }
    }
    return null;
  }

  function tryPlaceBundledPill(pillCtx, idxs, preferEnd) {
    const e0 = pillCtx.activeEdges[idxs[0]];
    const st = edgeStyle(e0);
    const bundledLabel = `${e0.verb.trim()} (${idxs.length}×)`;
    const rIdxs = idxs
      .map(ei => pillCtx.singletonRouteIdxByEdge.get(ei))
      .filter(v => v !== undefined);
    return tryPlacePill(pillCtx, bundledLabel, rIdxs, idxs, st.color, preferEnd);
  }

  function placeTrunkPill(pillCtx, item, routeIdx, unplacedBundleErrors) {
    const b = item.bundle;
    const st = edgeStyle(b.edgeSample);
    const preferEnd = b.kind === 'fan_in' ? 'dst' : 'src';
    let placed = tryPlacePill(pillCtx, b.label, [routeIdx], b.edgeIndices, st.color, preferEnd);
    if (!placed) {
      placed = tryPlacePill(pillCtx, b.label, pillCtx.bundleAllRouteIdxs.get(b.id) || [routeIdx], b.edgeIndices, st.color, null);
    }
    if (placed) {
      pillCtx.placedPills.push(placed);
    } else {
      unplacedBundleErrors.push({ label: b.label, edgeIndices: b.edgeIndices, hubId: b.hubId });
    }
  }

  function placeSingletonPill(pillCtx, item, routeIdx, unplacedEdgeIndices) {
    const e = pillCtx.activeEdges[item.edgeIdx];
    const verb = (e.verb || '').trim();
    if (!verb) return;
    const placed = tryPlacePill(pillCtx, verb, [routeIdx], [item.edgeIdx], edgeStyle(e).color, null);
    if (placed) {
      pillCtx.placedPills.push(placed);
    } else {
      unplacedEdgeIndices.push(item.edgeIdx);
    }
  }

  function placeInitialPills(pillCtx) {
    const unplacedEdgeIndices = [];
    const unplacedBundleErrors = [];
    for (let i = 0; i < pillCtx.routeItems.length; i++) {
      const item = pillCtx.routeItems[i];
      if (item.isTrunk && item.bundle) {
        placeTrunkPill(pillCtx, item, i, unplacedBundleErrors);
      } else if (!item.bundleId && item.edgeIdx !== undefined) {
        placeSingletonPill(pillCtx, item, i, unplacedEdgeIndices);
      }
    }
    return { unplacedEdgeIndices, unplacedBundleErrors };
  }

  function bundleRemainingPills(pillCtx, indices, keyFn, preferEnd) {
    const groups = new Map();
    for (const i of indices) {
      const e = pillCtx.activeEdges[i];
      const verb = (e.verb || '').trim();
      addMapListEntry(groups, keyFn(e, verb, edgeStyle(e)), i);
    }
    const leftover = [];
    for (const idxs of groups.values()) {
      if (idxs.length >= 2) {
        const placed = tryPlaceBundledPill(pillCtx, idxs, preferEnd);
        if (placed) {
          pillCtx.placedPills.push(placed);
          continue;
        }
      }
      leftover.push(...idxs);
    }
    return leftover;
  }

  function indexSingletonGroups(pillCtx) {
    const allByDst = new Map();
    const allBySrc = new Map();
    for (const ei of pillCtx.singletonRouteIdxByEdge.keys()) {
      const e = pillCtx.activeEdges[ei];
      const verb = (e.verb || '').trim();
      if (!verb) continue;
      const tag = edgeStyle(e).tag;
      addMapListEntry(allByDst, `${e.to}|${verb}|${tag}`, ei);
      addMapListEntry(allBySrc, `${e.from}|${verb}|${tag}`, ei);
    }
    return { allByDst, allBySrc };
  }

  function tryReclaimGroup(pillCtx, groupIdxs, preferEnd) {
    if (!groupIdxs || groupIdxs.length < 2) return false;
    const groupSet = new Set(groupIdxs);
    const removed = [];
    for (let k = pillCtx.placedPills.length - 1; k >= 0; k--) {
      if (pillCtx.placedPills[k].edgeIndices.some(ei => groupSet.has(ei))) {
        removed.push(pillCtx.placedPills.splice(k, 1)[0]);
      }
    }
    const placed = tryPlaceBundledPill(pillCtx, groupIdxs, preferEnd);
    if (placed) {
      pillCtx.placedPills.push(placed);
      return true;
    }
    for (const r of removed) pillCtx.placedPills.push(r);
    return false;
  }

  function addAllToSet(target, items) {
    for (const item of items) target.add(item);
  }

  function reclaimSiblingPills(pillCtx, stillUnplaced) {
    if (stillUnplaced.length === 0) return stillUnplaced;
    const { allByDst, allBySrc } = indexSingletonGroups(pillCtx);
    const resolved = new Set();
    for (const i of stillUnplaced) {
      if (resolved.has(i)) continue;
      const e = pillCtx.activeEdges[i];
      const verb = (e.verb || '').trim();
      const tag = edgeStyle(e).tag;
      const dstGroup = allByDst.get(`${e.to}|${verb}|${tag}`);
      const srcGroup = allBySrc.get(`${e.from}|${verb}|${tag}`);
      if (tryReclaimGroup(pillCtx, dstGroup, 'dst')) {
        addAllToSet(resolved, dstGroup);
      } else if (tryReclaimGroup(pillCtx, srcGroup, 'src')) {
        addAllToSet(resolved, srcGroup);
      }
    }
    return stillUnplaced.filter(i => !resolved.has(i));
  }

  function placeAllViewPills(pillCtx) {
    // Place edge verb pills (zero truncation + shared-hub bundling).
    const { unplacedEdgeIndices, unplacedBundleErrors } = placeInitialPills(pillCtx);
    let stillUnplaced = [];
    if (unplacedEdgeIndices.length > 0) {
      const afterIn = bundleRemainingPills(pillCtx, unplacedEdgeIndices, (e, verb, st) => `${e.to}|${verb}|${st.tag}`, 'dst');
      stillUnplaced = bundleRemainingPills(pillCtx, afterIn, (e, verb, st) => `${e.from}|${verb}|${st.tag}`, 'src');
    }
    stillUnplaced = reclaimSiblingPills(pillCtx, stillUnplaced);
    return { stillUnplaced, unplacedBundleErrors };
  }

  function auditCardOverlaps(viewId, cardEntries, headerEntries, overlaps, collisions) {
    for (let i = 0; i < cardEntries.length; i++) {
      for (let j = i + 1; j < cardEntries.length; j++) {
        if (rectsOverlap(cardEntries[i].stageRect, cardEntries[j].stageRect, 0)) {
          const msg = `card overlap: ${cardEntries[i].id} vs ${cardEntries[j].id}`;
          overlaps.push(msg);
          collisions.push({
            view_id: viewId,
            kind: 'card_collision',
            subjects: [cardEntries[i].id, cardEntries[j].id],
            detail: msg
          });
        }
      }
      for (const he of headerEntries) {
        if (rectsOverlap(cardEntries[i].stageRect, he.stageRect, 0)) {
          const msg = `card_header overlap: ${cardEntries[i].id} vs ${he.id}`;
          overlaps.push(msg);
          collisions.push({
            view_id: viewId,
            kind: 'header_card_collision',
            subjects: [cardEntries[i].id, he.id],
            detail: msg
          });
        }
      }
    }
  }

  function auditContainerOverlaps(viewId, boxEntries, overlaps, collisions) {
    for (let i = 0; i < boxEntries.length; i++) {
      for (let j = i + 1; j < boxEntries.length; j++) {
        const a = boxEntries[i].stageRect;
        const b = boxEntries[j].stageRect;
        if (rectsOverlap(a, b, 0) && !boxContains(a, b, 0.5) && !boxContains(b, a, 0.5)) {
          const msg = `container partial overlap: ${boxEntries[i].id} vs ${boxEntries[j].id}`;
          overlaps.push(msg);
          collisions.push({
            view_id: viewId,
            kind: 'container_overlap',
            subjects: [boxEntries[i].id, boxEntries[j].id],
            detail: msg
          });
        }
      }
    }
  }

  function auditSegmentCardPierces(viewId, s, sd, cardEntries, overlaps, collisions) {
    const isInterior = s.segIdx > 0 && s.segIdx < s.totalSegs - 1;
    for (const ce of cardEntries) {
      if (ce.stageRect === sd.srcRect || ce.stageRect === sd.dstRect) {
        if (isInterior && segIntersectsRect(s.x1, s.y1, s.x2, s.y2, ce.stageRect, -4)) {
          const msg = `wire_endpoint_pierce: edge ${sd.label} pierces ${ce.id}`;
          overlaps.push(msg);
          collisions.push({ view_id: viewId, kind: 'wire_card_collision', subjects: [sd.label, ce.id], detail: msg });
        }
        continue;
      }
      if (segIntersectsRect(s.x1, s.y1, s.x2, s.y2, ce.stageRect, -2)) {
        const msg = `wire_card_pierce: edge ${sd.label} (${s.x1.toFixed(0)},${s.y1.toFixed(0)}->${s.x2.toFixed(0)},${s.y2.toFixed(0)} seg ${s.segIdx}/${s.totalSegs}) pierces ${ce.id} (${ce.stageRect.left.toFixed(0)},${ce.stageRect.top.toFixed(0)},${ce.stageRect.right.toFixed(0)},${ce.stageRect.bottom.toFixed(0)})`;
        overlaps.push(msg);
        collisions.push({ view_id: viewId, kind: 'wire_card_collision', subjects: [sd.label, ce.id], detail: msg });
      }
    }
  }

  function auditWirePierces(viewId, auditCtx, overlaps, collisions) {
    for (const s of auditCtx.finalSegs) {
      const sd = auditCtx.routeMetaList[s.routeIdx];
      auditSegmentCardPierces(viewId, s, sd, auditCtx.cardEntries, overlaps, collisions);
      for (const he of auditCtx.headerEntries) {
        if (segIntersectsRect(s.x1, s.y1, s.x2, s.y2, he.stageRect, -1)) {
          const msg = `wire_header_pierce: edge ${sd.label} pierces ${he.id}`;
          overlaps.push(msg);
          collisions.push({ view_id: viewId, kind: 'wire_header_collision', subjects: [sd.label, he.id], detail: msg });
        }
      }
    }
  }

  function routeLabel(routeMetaList, idx) {
    const sd = routeMetaList[idx];
    return sd ? sd.label : String(idx);
  }

  function auditSinglePillCollisions(viewId, p, auditCtx, overlaps, collisions) {
    const { finalSegs, routeMetaList, endpointArrowRects, borderBands } = auditCtx;
    for (const s of finalSegs) {
      if (s !== p.owningSeg && segIntersectsRect(s.x1, s.y1, s.x2, s.y2, p.rect, 3)) {
        const label = routeLabel(routeMetaList, s.routeIdx);
        const msg = `pill_wire_collision: pill "${p.label}" overlaps wire ${label}`;
        overlaps.push(msg);
        collisions.push({ view_id: viewId, kind: 'pill_wire_collision', subjects: [p.label, label], detail: msg });
        break;
      }
    }
    for (const ar of endpointArrowRects) {
      if (rectsOverlap(p.rect, ar, 1.5)) {
        const label = routeLabel(routeMetaList, ar.routeIdx);
        const msg = `pill_wire_collision: pill "${p.label}" overlaps arrowhead on ${label}`;
        overlaps.push(msg);
        collisions.push({ view_id: viewId, kind: 'pill_wire_collision', subjects: [p.label, label], detail: msg });
        break;
      }
    }
    for (const bb of borderBands) {
      if (rectsOverlap(p.rect, bb, 4.5)) {
        const msg = `pill_border_collision: pill "${p.label}" overlaps container border`;
        overlaps.push(msg);
        collisions.push({ view_id: viewId, kind: 'pill_border_collision', subjects: [p.label], detail: msg });
        break;
      }
    }
  }

  function auditPillCollisions(viewId, auditCtx, overlaps, collisions) {
    for (const p of auditCtx.placedPills) {
      auditSinglePillCollisions(viewId, p, auditCtx, overlaps, collisions);
    }
  }

  function auditUnplacedLabels(viewId, activeEdges, unplacedBundleErrors, stillUnplaced, overlaps, collisions) {
    const unplacedLabels = [];
    for (const ub of unplacedBundleErrors) {
      const msg = `unplaced_label: bundle ${ub.hubId} (${ub.label})`;
      unplacedLabels.push(msg);
      overlaps.push(msg);
      collisions.push({
        view_id: viewId,
        kind: 'unplaced_label',
        subjects: [ub.hubId],
        detail: msg
      });
    }
    for (const i of stillUnplaced) {
      const e = activeEdges[i];
      const msg = `unplaced_label: ${e.from}->${e.to} (${(e.verb || '').trim()})`;
      unplacedLabels.push(msg);
      overlaps.push(msg);
      collisions.push({
        view_id: viewId,
        kind: 'unplaced_label',
        subjects: [e.from + '->' + e.to],
        detail: msg
      });
    }
    return unplacedLabels;
  }

  function routeAndAuditView(sheet, rawEdges, workspace) {
    const stage = sheet.querySelector('.diagram-stage');
    const svg = sheet.querySelector('svg.connector-layer');
    const viewId = sheet.dataset.viewId || 'view';
    const stageRect = stage.getBoundingClientRect();
    const sheetRect = sheet.getBoundingClientRect();
    const stageW = stage.scrollWidth;
    const stageH = stage.scrollHeight;

    const isDense = sheet.dataset.dense === '1';
    const pillFontSize = isDense ? 10.5 : PILL_FONT_SIZE;
    const pillHeight = isDense ? 17 : PILL_HEIGHT;

    const geom = measureStageGeometry(stage, stageRect);
    const { xs, ys } = buildChannelCoords(geom.cardRects, geom.headerRects, geom.boxRects, stageW, stageH);
    const gridMasks = buildStaticGridMasks(xs, ys, geom.cardRects, geom.headerRects, geom.boxEntries, geom.boxTopBands);
    const viewCtx = { ...geom, xs, ys, gridMasks, stageW, stageH };

    const activeEdges = rawEdges.filter(e => geom.nodeRects.has(e.from) && geom.nodeRects.has(e.to));
    const plan = buildRoutePlan(activeEdges, viewCtx);
    const routed = routeAllItems(plan.routeItems, viewCtx, workspace);

    const placedPills = [];
    const pillCtx = {
      ...viewCtx,
      ...plan,
      ...routed,
      activeEdges,
      placedPills,
      pillFontSize,
      pillHeight
    };
    const { stillUnplaced, unplacedBundleErrors } = placeAllViewPills(pillCtx);

    // Geometry Audit (in stage coordinates).
    const overlaps = [];
    const collisions = [];
    auditCardOverlaps(viewId, geom.cardEntries, geom.headerEntries, overlaps, collisions);
    auditContainerOverlaps(viewId, geom.boxEntries, overlaps, collisions);
    const auditCtx = { ...geom, ...routed, placedPills };
    auditWirePierces(viewId, auditCtx, overlaps, collisions);
    auditPillCollisions(viewId, auditCtx, overlaps, collisions);
    const unplacedLabels = auditUnplacedLabels(viewId, activeEdges, unplacedBundleErrors, stillUnplaced, overlaps, collisions);

    return {
      viewId,
      stageW,
      stageH,
      stageRect,
      sheetRect,
      svg,
      finalSegs: routed.finalSegs,
      verticalSegs: routed.verticalSegs,
      junctions: plan.junctions,
      placedPills,
      pillFontSize,
      unplacedLabels,
      unplacedCount: stillUnplaced.length + unplacedBundleErrors.length,
      overlaps,
      clippedTexts: [],
      overflowTexts: [],
      collisions
    };
  }

  function segmentStrokeWidth(seg, baseWidth) {
    if (seg.isTrunk) return Math.max(2.6, baseWidth + 0.8);
    if (seg.bundleId) return Math.max(1.3, baseWidth - 0.2);
    return baseWidth;
  }

  function applyStrokeDecorations(el, st, heuristic, isLast, viewId) {
    if (st.dash) {
      el.setAttribute('stroke-dasharray', st.dash);
    } else if (heuristic) {
      el.setAttribute('stroke-dasharray', '3 3');
    }
    if (isLast) {
      el.setAttribute('marker-end', `url(#arrow-${viewId}-${st.tag})`);
    }
  }

  function buildArrowDefs(viewId) {
    const defs = document.createElementNS(SVG_NS, 'defs');
    for (const [tag, st] of Object.entries(SEMANTIC_STYLES)) {
      const mk = document.createElementNS(SVG_NS, 'marker');
      mk.setAttribute('id', `arrow-${viewId}-${tag}`);
      mk.setAttribute('viewBox', '0 0 10 10');
      mk.setAttribute('refX', '8.5');
      mk.setAttribute('refY', '5');
      mk.setAttribute('markerWidth', '6.5');
      mk.setAttribute('markerHeight', '6.5');
      mk.setAttribute('orient', 'auto-start-reverse');
      const poly = document.createElementNS(SVG_NS, 'path');
      poly.setAttribute('d', 'M 0 1.5 L 9 5 L 0 8.5 z');
      poly.setAttribute('fill', st.color);
      mk.appendChild(poly);
      defs.appendChild(mk);
    }
    return defs;
  }

  function paintWirePass(wireLayer, finalSegs, verticalSegs, viewId) {
    const bridgedHorizontals = [];
    for (const seg of finalSegs) {
      const e = seg.edgeSample;
      const st = edgeStyle(e);
      const strokeW = segmentStrokeWidth(seg, st.width);
      const isLast = seg.segIdx === seg.totalSegs - 1 && seg.hasArrow;
      if (seg.isH) {
        const d = buildHorizontalJumperPath({ x: seg.x1, y: seg.y1 }, { x: seg.x2, y: seg.y2 }, verticalSegs, seg.routeIdx, seg.bundleId);
        if (d) {
          bridgedHorizontals.push({ seg, d, st, strokeW, isLast, heuristic: e.heuristic });
          continue;
        }
      }
      const line = document.createElementNS(SVG_NS, 'line');
      line.setAttribute('x1', seg.x1.toFixed(1));
      line.setAttribute('y1', seg.y1.toFixed(1));
      line.setAttribute('x2', seg.x2.toFixed(1));
      line.setAttribute('y2', seg.y2.toFixed(1));
      line.setAttribute('stroke', st.color);
      line.setAttribute('stroke-width', String(strokeW));
      line.setAttribute('stroke-linecap', 'round');
      applyStrokeDecorations(line, st, e.heuristic, isLast, viewId);
      wireLayer.appendChild(line);
    }
    return bridgedHorizontals;
  }

  function paintBridgePass(bridgeLayer, bridgedHorizontals, viewId) {
    for (const bh of bridgedHorizontals) {
      const halo = document.createElementNS(SVG_NS, 'path');
      halo.setAttribute('d', bh.d);
      halo.setAttribute('fill', 'none');
      halo.setAttribute('stroke', '#FFFFFF');
      halo.setAttribute('stroke-width', String(bh.strokeW + 3.8));
      halo.setAttribute('stroke-linecap', 'round');
      halo.dataset.bridgeHalo = '1';
      bridgeLayer.appendChild(halo);

      const path = document.createElementNS(SVG_NS, 'path');
      path.setAttribute('d', bh.d);
      path.setAttribute('fill', 'none');
      path.setAttribute('stroke', bh.st.color);
      path.setAttribute('stroke-width', String(bh.strokeW));
      path.setAttribute('stroke-linecap', 'round');
      path.dataset.bridge = '1';
      applyStrokeDecorations(path, bh.st, bh.heuristic, bh.isLast, viewId);
      bridgeLayer.appendChild(path);
    }
  }

  function paintJunctionPass(junctionLayer, junctions) {
    for (const jpt of junctions || []) {
      const dot = document.createElementNS(SVG_NS, 'circle');
      dot.setAttribute('cx', jpt.x.toFixed(1));
      dot.setAttribute('cy', jpt.y.toFixed(1));
      dot.setAttribute('r', '3.8');
      dot.setAttribute('fill', jpt.color);
      dot.setAttribute('stroke', '#FFFFFF');
      dot.setAttribute('stroke-width', '1.2');
      junctionLayer.appendChild(dot);
    }
  }

  function paintPillPass(pillLayer, placedPills, pillFontSize) {
    const pfSize = pillFontSize || PILL_FONT_SIZE;
    const textYOffset = pfSize * 0.34;
    for (const p of placedPills) {
      const g = document.createElementNS(SVG_NS, 'g');
      g.setAttribute('class', 'verb-pill');

      const bg = document.createElementNS(SVG_NS, 'rect');
      bg.setAttribute('x', p.rect.left.toFixed(1));
      bg.setAttribute('y', p.rect.top.toFixed(1));
      bg.setAttribute('width', p.rect.width.toFixed(1));
      bg.setAttribute('height', p.rect.height.toFixed(1));
      bg.setAttribute('rx', '4');
      bg.setAttribute('fill', '#FFFFFF');
      bg.setAttribute('stroke', p.color);
      bg.setAttribute('stroke-width', '1.1');
      g.appendChild(bg);

      const txt = document.createElementNS(SVG_NS, 'text');
      txt.setAttribute('x', p.cx.toFixed(1));
      txt.setAttribute('y', (p.cy + textYOffset).toFixed(1));
      txt.setAttribute('text-anchor', 'middle');
      txt.setAttribute('font-family', "'Inter', sans-serif");
      txt.setAttribute('font-size', String(pfSize));
      txt.setAttribute('font-weight', '600');
      txt.setAttribute('fill', '#0f172a');
      txt.textContent = p.label;
      g.appendChild(txt);

      pillLayer.appendChild(g);
    }
  }

  function paintViewSVG(res) {
    const { svg, viewId, stageW, stageH, finalSegs, verticalSegs, junctions, placedPills, pillFontSize } = res;
    svg.setAttribute('viewBox', `0 0 ${stageW.toFixed(1)} ${stageH.toFixed(1)}`);
    svg.setAttribute('width', stageW.toFixed(1));
    svg.setAttribute('height', stageH.toFixed(1));
    svg.innerHTML = '';

    svg.appendChild(buildArrowDefs(viewId));
    const wireLayer = document.createElementNS(SVG_NS, 'g');
    const bridgeLayer = document.createElementNS(SVG_NS, 'g');
    const junctionLayer = document.createElementNS(SVG_NS, 'g');
    const pillLayer = document.createElementNS(SVG_NS, 'g');
    svg.appendChild(wireLayer);
    svg.appendChild(bridgeLayer);
    svg.appendChild(junctionLayer);
    svg.appendChild(pillLayer);

    const bridgedHorizontals = paintWirePass(wireLayer, finalSegs, verticalSegs, viewId);
    paintBridgePass(bridgeLayer, bridgedHorizontals, viewId);
    paintJunctionPass(junctionLayer, junctions);
    paintPillPass(pillLayer, placedPills, pillFontSize);
  }

  function toReportBox(id, r) {
    return {
      id,
      x: Math.round(r.left * 100) / 100,
      y: Math.round(r.top * 100) / 100,
      width: Math.round(r.width * 100) / 100,
      height: Math.round(r.height * 100) / 100
    };
  }

  function measureViewBoxes(sheet, origin) {
    const sr = origin || sheet.getBoundingClientRect();
    const stage = sheet.querySelector('.diagram-stage');
    const stageRect = stage ? stage.getBoundingClientRect() : sr;
    const scope = stage || sheet;

    const nodeBoxes = Array.from(scope.querySelectorAll('.arch-card')).map(card =>
      toReportBox(card.dataset.componentId || '', relRect(card, sr))
    );
    const containerBoxes = [];
    const badgeBoxes = [];
    for (const box of scope.querySelectorAll('.arch-box')) {
      const cid = box.dataset.containerId || '';
      containerBoxes.push(toReportBox(cid, relRect(box, sr)));
      // Map badge_boxes strictly to .arch-box-header strips so BadgeBoxes never overlap NodeBoxes.
      const hdr = box.querySelector(':scope > .arch-box-header');
      if (hdr) {
        badgeBoxes.push(toReportBox(cid + ':header', headerContentRect(hdr, sr)));
      }
    }

    const dx = stageRect.left - sr.left;
    const dy = stageRect.top - sr.top;
    const svg = sheet.querySelector('svg.connector-layer');
    const labelBoxes = [];
    if (svg) {
      svg.querySelectorAll('g.verb-pill').forEach((g, pi) => {
        const rc = g.querySelector('rect');
        if (!rc) return;
        const txt = g.querySelector('text');
        const lbl = txt ? txt.textContent : '';
        const rx = Number.parseFloat(rc.getAttribute('x') || '0');
        const ry = Number.parseFloat(rc.getAttribute('y') || '0');
        const rw = Number.parseFloat(rc.getAttribute('width') || '0');
        const rh = Number.parseFloat(rc.getAttribute('height') || '0');
        labelBoxes.push({
          id: `pill_${pi}_${lbl}`,
          x: Math.round((rx + dx) * 100) / 100,
          y: Math.round((ry + dy) * 100) / 100,
          width: Math.round(rw * 100) / 100,
          height: Math.round(rh * 100) / 100
        });
      });
    }

    return { sr, nodeBoxes, containerBoxes, badgeBoxes, labelBoxes };
  }

  function buildViewReportDIP(sheet, res) {
    const measured = measureViewBoxes(sheet);
    const sr = measured.sr;
    const w = Math.ceil(sr.width);
    const h = Math.ceil(sr.height);
    const titleEl = sheet.querySelector('.view-title');
    const title = titleEl ? titleEl.textContent.trim() : res.viewId;

    return {
      viewRect: {
        view_id: res.viewId,
        index: Number(sheet.dataset.viewIndex || '0'),
        x: Math.round((sr.left + window.scrollX) * 100) / 100,
        y: Math.round((sr.top + window.scrollY) * 100) / 100,
        width: w,
        height: h
      },
      viewReport: {
        id: res.viewId,
        title,
        png_path: '',
        width_px: w,
        height_px: h,
        aspect_ratio: Math.round((w / Math.max(1, h)) * 1000) / 1000,
        node_boxes: measured.nodeBoxes,
        container_boxes: measured.containerBoxes,
        label_boxes: measured.labelBoxes,
        badge_boxes: measured.badgeBoxes,
        overlaps: res.overlaps,
        clipped_texts: res.clippedTexts,
        overflow_texts: res.overflowTexts,
        unplaced_labels: res.unplacedLabels,
        status: res.collisions.length === 0 ? 'PASS' : 'FAIL'
      }
    };
  }

  function applyPrintScale(sheet) {
    const rect = sheet.getBoundingClientRect();
    const w = Math.max(1, rect.width, sheet.scrollWidth);
    const h = Math.max(1, rect.height, sheet.scrollHeight);
    sheet.style.setProperty('--print-w', Math.ceil(w) + 'px');
    sheet.style.setProperty('--print-h', Math.ceil(h) + 'px');
    const scale = Math.min(1, PRINT_MAX_HEIGHT / h, PRINT_MAX_WIDTH / w);
    if (scale < 1) {
      sheet.style.setProperty('--print-scale', scale.toFixed(4));
    }
  }

  function measureViewRects() {
    const sheets = document.querySelectorAll('.view-sheet');
    const rects = [];
    sheets.forEach((sheet, idx) => {
      const r = sheet.getBoundingClientRect();
      rects.push({
        view_id: sheet.dataset.viewId || ('view_' + idx),
        index: idx,
        x: Math.round((r.left + window.scrollX) * 100) / 100,
        y: Math.round((r.top + window.scrollY) * 100) / 100,
        width: Math.ceil(r.width),
        height: Math.ceil(r.height)
      });
    });
    return rects;
  }

  function loadRoutingEdgesById() {
    const dataEl = document.getElementById('gcp-arch-routing-data');
    const routingViews = dataEl ? JSON.parse(dataEl.textContent) : [];
    const byID = {};
    for (const rv of routingViews) {
      if (rv?.id) byID[rv.id] = rv.edges || [];
    }
    return { routingViews, byID };
  }

  function needsBorderExpansion(res) {
    if (!res) return false;
    return res.unplacedCount > 0 || res.collisions.some(c => c.kind === 'pill_border_collision' || c.kind === 'pill_wire_collision');
  }

  function routeSingleSheet(sheet, rawEdges, viewId, workspace) {
    balanceViewAspectRatio(sheet);
    let res = null;
    for (let pass = 0; pass <= MAX_REFLOW_PASSES; pass++) {
      if (pass > 0) {
        applyCorridorSpacing(sheet, pass, needsBorderExpansion(res));
        balanceViewAspectRatio(sheet);
      }
      res = routeAndAuditView(sheet, rawEdges, workspace);
      if (res.collisions.length === 0 && res.unplacedCount === 0) {
        break;
      }
    }

    const textAudit = auditSheetText(sheet, viewId);
    res.clippedTexts = textAudit.clippedTexts;
    res.overflowTexts = textAudit.overflowTexts;
    for (const c of textAudit.collisions) {
      res.collisions.push(c);
    }

    paintViewSVG(res);
    applyPrintScale(sheet);
    return res;
  }

  async function runRouter() {
    if (document.documentElement.dataset.gcpArchRouted === '1') {
      const res = {
        ok: true,
        collisions: [],
        viewRects: measureViewRects(),
        viewReports: []
      };
      window.__GCP_ARCH_RENDER_RESULT__ = res;
      return res;
    }

    if (document.fonts) {
      await document.fonts.ready;
    }
    await nextDoubleAnimationFrame();

    const { routingViews, byID } = loadRoutingEdgesById();
    const sheets = Array.from(document.querySelectorAll('.view-sheet'));
    const allCollisions = [];
    const viewRects = [];
    const viewReports = [];
    const workspace = createAStarWorkspace();

    for (let idx = 0; idx < sheets.length; idx++) {
      const sheet = sheets[idx];
      const viewId = sheet.dataset.viewId || ('view_' + idx);
      const rawEdges = byID[viewId] || routingViews[idx]?.edges || [];
      const res = routeSingleSheet(sheet, rawEdges, viewId, workspace);
      const built = buildViewReportDIP(sheet, res);
      viewRects.push(built.viewRect);
      viewReports.push(built.viewReport);
      for (const c of res.collisions) {
        allCollisions.push(c);
      }
    }

    if (allCollisions.length > 0) {
      const failRes = {
        ok: false,
        collisions: allCollisions,
        viewRects,
        viewReports
      };
      window.__GCP_ARCH_RENDER_RESULT__ = failRes;
      return failRes;
    }

    document.documentElement.dataset.gcpArchRouted = '1';
    const okRes = {
      ok: true,
      collisions: [],
      viewRects,
      viewReports
    };
    window.__GCP_ARCH_RENDER_RESULT__ = okRes;
    return okRes;
  }

  function nextDoubleAnimationFrame() {
    return new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
  }

  function sheetViewRectDIP(sheet, fallbackIdx) {
    const sr = sheet.getBoundingClientRect();
    const rawIdx = sheet.dataset.viewIndex;
    const idx = rawIdx !== undefined && rawIdx !== '' ? Number(rawIdx) : fallbackIdx;
    return {
      view_id: sheet.dataset.viewId || ('view_' + fallbackIdx),
      index: idx,
      x: Math.max(0, Math.round((sr.left + window.scrollX) * 100) / 100),
      y: Math.max(0, Math.round((sr.top + window.scrollY) * 100) / 100),
      width: Math.ceil(sr.width),
      height: Math.ceil(sr.height)
    };
  }

  window.__GCP_ARCH_ISOLATE_VIEW__ = async function (targetIdx) {
    const root = document.querySelector('.document-root');
    const sheets = Array.from(document.querySelectorAll('.view-sheet'));
    if (root) {
      root.style.padding = '0';
      root.style.gap = '0';
      root.style.margin = '0';
    }
    sheets.forEach((s, idx) => {
      s.style.display = idx === targetIdx ? '' : 'none';
    });
    const sheet = sheets[targetIdx];
    if (!sheet) {
      return null;
    }
    window.scrollTo(0, 0);
    await nextDoubleAnimationFrame();
    return sheetViewRectDIP(sheet, targetIdx);
  };

  window.__GCP_ARCH_MEASURE_ISOLATED_VIEW__ = async function (targetIdx) {
    const sheets = document.querySelectorAll('.view-sheet');
    const sheet = sheets[targetIdx];
    if (!sheet) {
      return null;
    }
    window.scrollTo(0, 0);
    await nextDoubleAnimationFrame();

    const measured = measureViewBoxes(sheet);
    return {
      view_rect: sheetViewRectDIP(sheet, targetIdx),
      node_boxes: measured.nodeBoxes,
      container_boxes: measured.containerBoxes,
      badge_boxes: measured.badgeBoxes,
      label_boxes: measured.labelBoxes
    };
  };

  window.__GCP_ARCH_HIDE_VIEW__ = function (targetIdx) {
    const sheets = document.querySelectorAll('.view-sheet');
    if (sheets[targetIdx]) {
      sheets[targetIdx].style.display = 'none';
    }
  };

  window.__GCP_ARCH_RENDER_PROMISE__ = runRouter();
})();
