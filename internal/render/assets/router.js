(function () {
  'use strict';

  const FLOW_COLORS = {
    RUNTIME: '#1D4ED8',
    CICD: '#B45309',
    ADMIN: '#475569'
  };

  const CARD_CLEARANCE = 4;
  const BADGE_CLEARANCE = 4;
  const PILL_CLEARANCE = 4;
  const COLLINEAR_MIN_SEP = 6;
  const LANE_STEP = 8;
  const CORRIDOR_WALL_PAD = 12;
  const MAX_CORRIDOR_SIZE = 160;
  const MAX_REFLOW_PASSES = 2;
  const PORT_CORNER_MARGIN = 10;
  const PILL_HEIGHT = 15;
  const PILL_PAD_X = 5;
  const PILL_FONT_SIZE = 8.5;
  const BRIDGE_RADIUS = 4;
  const JUNCTION_RADIUS = 2.5;
  const SVG_NS = 'http://www.w3.org/2000/svg';

  const measureCtx = (function () {
    const canvas = document.createElement('canvas');
    const ctx = canvas.getContext('2d');
    if (ctx) {
      ctx.font = '500 ' + PILL_FONT_SIZE + 'px Inter, sans-serif';
    }
    return ctx;
  })();

  function measurePillWidth(label) {
    if (!label) return 0;
    if (measureCtx) {
      return Math.ceil(measureCtx.measureText(label).width) + PILL_PAD_X * 2;
    }
    return Math.ceil(label.length * 5.2) + PILL_PAD_X * 2;
  }

  function formatEdgeLabel(edge) {
    if (edge.footnote_num && edge.footnote_num > 0) {
      return edge.verb + ' [' + edge.footnote_num + ']';
    }
    return edge.verb;
  }

  function measureViewRects() {
    const sheets = document.querySelectorAll('.view-sheet');
    const rects = [];
    sheets.forEach(function (sheet, idx) {
      const r = sheet.getBoundingClientRect();
      rects.push({
        view_id: sheet.dataset.viewId || ('view_' + idx),
        index: idx,
        x: Math.round((r.left + window.scrollX) * 100) / 100,
        y: Math.round((r.top + window.scrollY) * 100) / 100,
        width: Math.round(r.width * 100) / 100,
        height: Math.round(r.height * 100) / 100
      });
    });
    return rects;
  }

  function rectsOverlap(a, b, pad) {
    return (
      a.left < b.right + pad - 0.5 &&
      a.right > b.left - pad + 0.5 &&
      a.top < b.bottom + pad - 0.5 &&
      a.bottom > b.top - pad + 0.5
    );
  }

  function segIntersectsRect(seg, rect, pad) {
    const rL = rect.left - pad;
    const rR = rect.right + pad;
    const rT = rect.top - pad;
    const rB = rect.bottom + pad;
    const minX = Math.min(seg.x1, seg.x2);
    const maxX = Math.max(seg.x1, seg.x2);
    const minY = Math.min(seg.y1, seg.y2);
    const maxY = Math.max(seg.y1, seg.y2);
    if (seg.isHorizontal) {
      return seg.y1 > rT + 0.5 && seg.y1 < rB - 0.5 && maxX > rL + 0.5 && minX < rR - 0.5;
    }
    return seg.x1 > rL + 0.5 && seg.x1 < rR - 0.5 && maxY > rT + 0.5 && minY < rB - 0.5;
  }

  function segPenetratesInterior(seg, rect) {
    const rL = rect.left + 1.5;
    const rR = rect.right - 1.5;
    const rT = rect.top + 1.5;
    const rB = rect.bottom - 1.5;
    if (rL >= rR || rT >= rB) return false;
    const minX = Math.min(seg.x1, seg.x2);
    const maxX = Math.max(seg.x1, seg.x2);
    const minY = Math.min(seg.y1, seg.y2);
    const maxY = Math.max(seg.y1, seg.y2);
    if (seg.isHorizontal) {
      return seg.y1 > rT && seg.y1 < rB && maxX > rL && minX < rR;
    }
    return seg.x1 > rL && seg.x1 < rR && maxY > rT && minY < rB;
  }

  function measureSheetGeometry(sheet) {
    const sr = sheet.getBoundingClientRect();
    const rel = function (el) {
      const r = el.getBoundingClientRect();
      return {
        left: r.left - sr.left,
        top: r.top - sr.top,
        right: r.right - sr.left,
        bottom: r.bottom - sr.top,
        width: r.width,
        height: r.height,
        cx: r.left - sr.left + r.width / 2,
        cy: r.top - sr.top + r.height / 2
      };
    };

    const cards = {};
    const cardList = [];
    sheet.querySelectorAll('.arch-card').forEach(function (el) {
      const box = rel(el);
      box.id = el.dataset.componentId;
      box.col = Number.parseInt(el.dataset.gridCol || '0', 10);
      box.row = Number.parseInt(el.dataset.gridRow || '0', 10);
      cards[box.id] = box;
      cardList.push(box);
    });

    const badges = [];
    sheet.querySelectorAll('.arch-container-badge').forEach(function (el) {
      const box = rel(el);
      box.id = el.dataset.containerId || el.textContent.trim();
      badges.push(box);
    });

    const vCorridors = {};
    sheet.querySelectorAll('.corridor-col').forEach(function (el) {
      const idx = Number.parseInt(el.dataset.corridorCol || '0', 10);
      const box = rel(el);
      box.index = idx;
      vCorridors[idx] = box;
    });

    const hCorridors = {};
    sheet.querySelectorAll('.corridor-row').forEach(function (el) {
      const idx = Number.parseInt(el.dataset.corridorRow || '0', 10);
      const box = rel(el);
      box.index = idx;
      hCorridors[idx] = box;
    });

    const waypoints = {};
    sheet.querySelectorAll('.edge-waypoint').forEach(function (el) {
      const eIdx = Number.parseInt(el.dataset.edgeIdx || '-1', 10);
      if (eIdx < 0) return;
      if (!waypoints[eIdx]) waypoints[eIdx] = [];
      waypoints[eIdx].push(rel(el));
    });

    return {
      width: sr.width,
      height: sr.height,
      cards: cards,
      cardList: cardList,
      badges: badges,
      vCorridors: vCorridors,
      hCorridors: hCorridors,
      waypoints: waypoints
    };
  }

  function compareBySortY(a, b) {
    if (Math.abs(a.sortY - b.sortY) > 0.5) return a.sortY - b.sortY;
    return a.order - b.order;
  }

  function compareBySortX(a, b) {
    if (Math.abs(a.sortX - b.sortX) > 0.5) return a.sortX - b.sortX;
    return a.order - b.order;
  }

  function resolvePortGroup(resolved, key, items, cards) {
    const parts = key.split('|');
    const cardID = parts[0];
    const side = parts[1];
    const card = cards[cardID];
    if (!card) return;

    items.sort(compareBySortY);

    const x = side === 'right' ? card.right : card.left;
    const minY = card.top + PORT_CORNER_MARGIN;
    const maxY = card.bottom - PORT_CORNER_MARGIN;
    const count = items.length;
    for (let i = 0; i < count; i++) {
      let y = card.cy;
      if (count > 1 && maxY > minY) {
        y = minY + ((maxY - minY) * i) / (count - 1);
      }
      resolved[items[i].portKey] = {
        x: x,
        y: y,
        side: side,
        cardID: cardID,
        minY: minY,
        maxY: maxY,
        col: card.col,
        groupTag: items[i].groupTag || items[i].portKey
      };
    }
  }

  function groupPortsByCorridor(resolved) {
    const byCorridor = {};
    for (const p of Object.values(resolved)) {
      const isRight = p.side === 'right';
      const vc = isRight ? p.col : (p.col - 1);
      if (vc < 0) continue;
      if (!byCorridor[vc]) byCorridor[vc] = { left: [], right: [] };
      (isRight ? byCorridor[vc].left : byCorridor[vc].right).push(p);
    }
    return byCorridor;
  }

  function separatePortPair(rp, lp, minSep) {
    if (rp.groupTag === lp.groupTag) return;
    const diff = rp.y - lp.y;
    if (Math.abs(diff) >= minSep - 0.1) return;
    const push = (minSep - Math.abs(diff)) / 2 + 0.5;
    const dir = diff >= 0 ? 1 : -1;
    const newRY = Math.max(rp.minY, Math.min(rp.maxY, rp.y + dir * push));
    const newLY = Math.max(lp.minY, Math.min(lp.maxY, lp.y - dir * push));
    if (Math.abs(newRY - newLY) >= minSep - 0.1) {
      rp.y = newRY;
      lp.y = newLY;
    } else if (rp.y + minSep <= rp.maxY) {
      rp.y = lp.y + minSep;
    } else if (rp.y - minSep >= rp.minY) {
      rp.y = lp.y - minSep;
    } else if (lp.y + minSep <= lp.maxY) {
      lp.y = rp.y + minSep;
    } else if (lp.y - minSep >= lp.minY) {
      lp.y = rp.y - minSep;
    }
  }

  function deconflictCorridorPorts(leftPorts, rightPorts, minSep) {
    for (let pass = 0; pass < 3; pass++) {
      for (const rp of rightPorts) {
        for (const lp of leftPorts) {
          separatePortPair(rp, lp, minSep);
        }
      }
    }
  }

  function assignPortCoords(portGroups, cards) {
    const resolved = {};
    for (const key of Object.keys(portGroups)) {
      resolvePortGroup(resolved, key, portGroups[key], cards);
    }

    // De-conflict opposing-side ports inside the same vertical corridor so horizontal trunks/taps
    // of distinct edges/buses never share a Y within COLLINEAR_MIN_SEP (6px).
    const byCorridor = groupPortsByCorridor(resolved);
    const minSep = COLLINEAR_MIN_SEP + 2; // 8px separation
    for (const corridor of Object.values(byCorridor)) {
      deconflictCorridorPorts(corridor.left, corridor.right, minSep);
    }

    return resolved;
  }

  function lanePosition(center, startBound, endBound, idx, count) {
    const usable = endBound - startBound;
    if (count <= 1) {
      return Math.max(startBound, Math.min(endBound, center));
    }
    const needed = (count - 1) * LANE_STEP;
    const step = needed <= usable ? LANE_STEP : Math.max(1, usable / (count - 1));
    const total = (count - 1) * step;
    const origin = startBound + (usable - total) / 2;
    return origin + idx * step;
  }

  function collectBlockedBands(hc, badges, minX, maxX) {
    const blocked = [];
    for (const b of badges) {
      const overlapX = b.right + BADGE_CLEARANCE > minX && b.left - BADGE_CLEARANCE < maxX;
      const overlapY = b.bottom + BADGE_CLEARANCE > hc.top && b.top - BADGE_CLEARANCE < hc.bottom;
      if (overlapX && overlapY) {
        blocked.push({ top: b.top - BADGE_CLEARANCE - 1, bottom: b.bottom + BADGE_CLEARANCE + 1 });
      }
    }
    blocked.sort((a, b) => a.top - b.top);
    return blocked;
  }

  function clearHorizontalBand(hc, badges, minX, maxX) {
    let top = hc.top + CORRIDOR_WALL_PAD;
    let bottom = hc.bottom - CORRIDOR_WALL_PAD;
    if (bottom < top) {
      top = hc.cy;
      bottom = hc.cy;
    }
    const blocked = collectBlockedBands(hc, badges, minX, maxX);
    if (blocked.length === 0) {
      return { top: top, bottom: bottom, badgeHeight: 0 };
    }
    const intervals = [];
    let cursor = hc.top + 6;
    const limit = hc.bottom - 6;
    let maxBadgeH = 0;
    for (const blk of blocked) {
      maxBadgeH = Math.max(maxBadgeH, blk.bottom - blk.top);
      if (blk.top > cursor) {
        intervals.push({ top: cursor, bottom: Math.min(limit, blk.top) });
      }
      cursor = Math.max(cursor, blk.bottom);
    }
    if (cursor < limit) {
      intervals.push({ top: cursor, bottom: limit });
    }
    if (intervals.length === 0) {
      return { top: top, bottom: bottom, badgeHeight: maxBadgeH };
    }
    intervals.sort((a, b) => (b.bottom - b.top) - (a.bottom - a.top));
    return { top: intervals[0].top, bottom: intervals[0].bottom, badgeHeight: maxBadgeH };
  }

  function pushNonNull(target, ...items) {
    for (const item of items) {
      if (item) target.push(item);
    }
  }

  function edgeSides(edge, cards) {
    const fc = cards[edge.from];
    const tc = cards[edge.to];
    if (!fc || !tc) return null;
    if (tc.col > fc.col) {
      return { fromSide: 'right', toSide: 'left', startVC: fc.col, endVC: tc.col - 1, dir: 1 };
    }
    if (tc.col < fc.col) {
      return { fromSide: 'left', toSide: 'right', startVC: fc.col - 1, endVC: tc.col, dir: -1 };
    }
    const side = edge.lateral_side || 'right';
    const latVC = edge.lateral_corridor;
    return { fromSide: side, toSide: side, startVC: latVC, endVC: latVC, dir: 0 };
  }

  function addPort(portGroups, cardID, side, spec) {
    const gk = cardID + '|' + side;
    if (!portGroups[gk]) portGroups[gk] = [];
    portGroups[gk].push({
      portKey: spec.portKey,
      sortY: spec.sortY,
      order: spec.order,
      groupTag: spec.groupTag || spec.portKey
    });
  }

  function registerVLane(vLaneGroups, corridorIdx, key, sortY, order) {
    if (!vLaneGroups[corridorIdx]) vLaneGroups[corridorIdx] = {};
    if (!vLaneGroups[corridorIdx][key]) {
      vLaneGroups[corridorIdx][key] = { key: key, sortY: sortY, order: order };
    }
  }

  function registerHLane(hLaneGroups, corridorIdx, key, spec) {
    if (!hLaneGroups[corridorIdx]) hLaneGroups[corridorIdx] = {};
    const existing = hLaneGroups[corridorIdx][key];
    if (!existing) {
      hLaneGroups[corridorIdx][key] = {
        key: key,
        sortX: spec.sortX,
        order: spec.order,
        minX: spec.minX,
        maxX: spec.maxX
      };
    } else {
      existing.minX = Math.min(existing.minX, spec.minX);
      existing.maxX = Math.max(existing.maxX, spec.maxX);
    }
  }

  function registerRemoteHLane(hLaneGroups, edge, fc, tc) {
    registerHLane(hLaneGroups, edge.waypoint_row, 'e:' + edge._idx, {
      sortX: Math.min(fc.cx, tc.cx),
      order: edge._idx,
      minX: Math.min(fc.right, tc.right),
      maxX: Math.max(fc.left, tc.left)
    });
  }

  function groupEdgesByBus(edges) {
    const busMap = {};
    const standalone = [];
    for (let idx = 0; idx < edges.length; idx++) {
      const edge = edges[idx];
      edge._idx = idx;
      if (edge.bus_group) {
        if (!busMap[edge.bus_group]) {
          busMap[edge.bus_group] = {
            id: edge.bus_group,
            kind: edge.bus_kind || 'out',
            flowKind: edge.flow_kind,
            edges: []
          };
        }
        busMap[edge.bus_group].edges.push(edge);
      } else {
        standalone.push(edge);
      }
    }
    return { busMap: busMap, standalone: standalone };
  }

  function registerBusEdgeBranch(e, bid, isOut, ctx) {
    const fc = ctx.cards[e.from];
    const tc = ctx.cards[e.to];
    const es = edgeSides(e, ctx.cards);
    const branchCard = isOut ? tc : fc;
    const y = branchCard ? branchCard.cy : 0;
    if (!es || !fc || !tc) return y;

    const busKey = 'bus:' + bid;
    if (isOut) {
      addPort(ctx.portGroups, e.to, es.toSide, { portKey: 'to:' + e._idx, sortY: fc.cy, order: e._idx, groupTag: busKey });
      if (Math.abs(tc.col - fc.col) > 1) {
        registerRemoteHLane(ctx.hLaneGroups, e, fc, tc);
        registerVLane(ctx.vLaneGroups, es.endVC, 'e_end:' + e._idx, tc.cy, e._idx);
      }
      return y;
    }

    addPort(ctx.portGroups, e.from, es.fromSide, { portKey: 'from:' + e._idx, sortY: tc.cy, order: e._idx, groupTag: busKey });
    if (Math.abs(tc.col - fc.col) > 1) {
      registerVLane(ctx.vLaneGroups, es.startVC, 'e_start:' + e._idx, fc.cy, e._idx);
      registerRemoteHLane(ctx.hLaneGroups, e, fc, tc);
    }
    return y;
  }

  function registerBusPortsAndLanes(bid, bus, ctx) {
    const first = bus.edges[0];
    const s0 = edgeSides(first, ctx.cards);
    if (!s0) return;

    const isOut = bus.kind === 'out';
    let avgY = 0;
    for (const e of bus.edges) {
      avgY += registerBusEdgeBranch(e, bid, isOut, ctx);
    }
    avgY /= bus.edges.length;

    const busKey = 'bus:' + bid;
    if (isOut) {
      addPort(ctx.portGroups, first.from, s0.fromSide, { portKey: busKey, sortY: avgY, order: first._idx, groupTag: busKey });
      registerVLane(ctx.vLaneGroups, s0.startVC, busKey, avgY, first._idx);
    } else {
      addPort(ctx.portGroups, first.to, s0.toSide, { portKey: busKey, sortY: avgY, order: first._idx, groupTag: busKey });
      registerVLane(ctx.vLaneGroups, s0.endVC, busKey, avgY, first._idx);
    }
  }

  function registerStandalonePortsAndLanes(edge, ctx) {
    const fc = ctx.cards[edge.from];
    const tc = ctx.cards[edge.to];
    const es = edgeSides(edge, ctx.cards);
    if (!fc || !tc || !es) return;

    const tag = 'edge:' + edge._idx;
    addPort(ctx.portGroups, edge.from, es.fromSide, { portKey: 'from:' + edge._idx, sortY: tc.cy, order: edge._idx, groupTag: tag });
    addPort(ctx.portGroups, edge.to, es.toSide, { portKey: 'to:' + edge._idx, sortY: fc.cy, order: edge._idx, groupTag: tag });

    if (es.dir === 0 || Math.abs(tc.col - fc.col) === 1) {
      registerVLane(ctx.vLaneGroups, es.startVC, 'e:' + edge._idx, (fc.cy + tc.cy) / 2, edge._idx);
      return;
    }
    registerVLane(ctx.vLaneGroups, es.startVC, 'e_start:' + edge._idx, fc.cy, edge._idx);
    registerRemoteHLane(ctx.hLaneGroups, edge, fc, tc);
    registerVLane(ctx.vLaneGroups, es.endVC, 'e_end:' + edge._idx, tc.cy, edge._idx);
  }

  function collectPortsAndLanes(busMap, standalone, cards) {
    const ctx = {
      cards: cards,
      portGroups: {},
      vLaneGroups: {},
      hLaneGroups: {}
    };
    const busIDs = Object.keys(busMap).sort((a, b) => a.localeCompare(b));
    for (const bid of busIDs) {
      registerBusPortsAndLanes(bid, busMap[bid], ctx);
    }
    for (const edge of standalone) {
      registerStandalonePortsAndLanes(edge, ctx);
    }
    return {
      portGroups: ctx.portGroups,
      vLaneGroups: ctx.vLaneGroups,
      hLaneGroups: ctx.hLaneGroups
    };
  }

  function assignVerticalLanes(vLaneGroups, vCorridors) {
    const vLaneX = {};
    const vLaneCounts = {};
    for (const cKey of Object.keys(vLaneGroups)) {
      const cIdx = Number.parseInt(cKey, 10);
      const vc = vCorridors[cIdx];
      if (!vc) continue;
      const items = Object.values(vLaneGroups[cKey]).sort(compareBySortY);
      vLaneCounts[cIdx] = items.length;
      const leftBound = vc.left + CORRIDOR_WALL_PAD;
      const rightBound = vc.right - CORRIDOR_WALL_PAD;
      vLaneX[cIdx] = {};
      for (let i = 0; i < items.length; i++) {
        vLaneX[cIdx][items[i].key] = lanePosition(vc.cx, leftBound, rightBound, i, items.length);
      }
    }
    return { vLaneX: vLaneX, vLaneCounts: vLaneCounts };
  }

  function assignHorizontalLanes(hLaneGroups, hCorridors, badges) {
    const hLaneY = {};
    const hLaneCounts = {};
    const hBadgeReserve = {};
    for (const rKey of Object.keys(hLaneGroups)) {
      const rIdx = Number.parseInt(rKey, 10);
      const hc = hCorridors[rIdx];
      if (!hc) continue;
      const items = Object.values(hLaneGroups[rKey]).sort(compareBySortX);
      hLaneCounts[rIdx] = items.length;
      let minX = Infinity;
      let maxX = -Infinity;
      for (const it of items) {
        minX = Math.min(minX, it.minX);
        maxX = Math.max(maxX, it.maxX);
      }
      const band = clearHorizontalBand(hc, badges, minX, maxX);
      hBadgeReserve[rIdx] = band.badgeHeight;
      hLaneY[rIdx] = {};
      const centerY = (band.top + band.bottom) / 2;
      for (let i = 0; i < items.length; i++) {
        hLaneY[rIdx][items[i].key] = lanePosition(centerY, band.top, band.bottom, i, items.length);
      }
    }
    return { hLaneY: hLaneY, hLaneCounts: hLaneCounts, hBadgeReserve: hBadgeReserve };
  }

  function pushSeg(segments, x1, y1, x2, y2, meta) {
    if (Math.abs(x1 - x2) < 0.25 && Math.abs(y1 - y2) < 0.25) return null;
    const isH = Math.abs(y1 - y2) < 0.25;
    const seg = {
      x1: x1, y1: y1, x2: x2, y2: y2,
      isHorizontal: isH,
      isVertical: !isH,
      edgeIdx: meta.edgeIdx,
      busGroup: meta.busGroup || '',
      fromID: meta.fromID,
      toID: meta.toID,
      flowKind: meta.flowKind,
      inferred: !!meta.inferred,
      isTerminalFrom: !!meta.isTerminalFrom,
      isTerminalTo: !!meta.isTerminalTo,
      corridorCol: meta.corridorCol,
      corridorRow: meta.corridorRow
    };
    segments.push(seg);
    return seg;
  }

  function edgeSegMeta(edge, busGroup, extra) {
    return {
      edgeIdx: edge._idx,
      busGroup: busGroup || '',
      fromID: edge.from,
      toID: edge.to,
      flowKind: edge.flow_kind,
      inferred: edge.provenance === 'INFERRED',
      ...extra
    };
  }

  function recordPillNeed(vPillReserve, corridorCol, labelW) {
    if (corridorCol === undefined || corridorCol === null) return;
    vPillReserve[corridorCol] = Math.max(vPillReserve[corridorCol] || 0, labelW + CARD_CLEARANCE * 2 + 2);
  }

  function computeBusSpineX(routeCtx, vcIdx, bid, preferLeft) {
    const vc = routeCtx.geom.vCorridors[vcIdx];
    // Bias single-spine bus slightly toward source/target side to open horizontal tap space for verb pills.
    if (routeCtx.vLaneCounts[vcIdx] === 1 && vc.width >= 72) {
      return preferLeft ? (vc.left + CORRIDOR_WALL_PAD + 4) : (vc.right - CORRIDOR_WALL_PAD - 4);
    }
    return routeCtx.vLaneX[vcIdx]['bus:' + bid];
  }

  function finishBusSpine(routeCtx, spineX, spineYs, trunkY, meta, color) {
    const minY = Math.min(...spineYs);
    const maxY = Math.max(...spineYs);
    if (maxY - minY <= 0.5) return;
    pushSeg(routeCtx.segments, spineX, minY, spineX, maxY, meta);
    if (trunkY > minY + 1 && trunkY < maxY - 1) {
      routeCtx.junctions.push({ x: spineX, y: trunkY, color: color });
    }
  }

  function buildBusTrunk(bid, bus, es0, allInferred, routeCtx) {
    const isOut = bus.kind === 'out';
    const first = bus.edges[0];
    const trunkPort = routeCtx.ports['bus:' + bid];
    const vcIdx = isOut ? es0.startVC : es0.endVC;
    const vc = routeCtx.geom.vCorridors[vcIdx];
    if (!trunkPort || !vc) return null;

    const preferLeft = isOut ? (es0.dir >= 0) : (es0.dir < 0);
    const spineX = computeBusSpineX(routeCtx, vcIdx, bid, preferLeft);
    const trunkMeta = {
      edgeIdx: first._idx,
      busGroup: bid,
      fromID: isOut ? first.from : '',
      toID: isOut ? '' : first.to,
      flowKind: bus.flowKind,
      inferred: allInferred,
      corridorCol: vcIdx
    };
    return { isOut: isOut, trunkPort: trunkPort, vcIdx: vcIdx, spineX: spineX, trunkMeta: trunkMeta };
  }

  function routeAdjacentBusBranch(edge, branchPort, spine, pillW, routeCtx) {
    spine.spineYs.push(branchPort.y);
    const x1 = spine.isOut ? spine.spineX : branchPort.x;
    const x2 = spine.isOut ? branchPort.x : spine.spineX;
    const extra = spine.isOut
      ? { isTerminalTo: true, corridorCol: spine.vcIdx }
      : { isTerminalFrom: true, corridorCol: spine.vcIdx };
    const tap = pushSeg(routeCtx.segments, x1, branchPort.y, x2, branchPort.y, edgeSegMeta(edge, spine.bid, extra));
    recordPillNeed(routeCtx.vPillReserve, spine.vcIdx, pillW);
    routeCtx.junctions.push({ x: spine.spineX, y: branchPort.y, color: spine.color });
    const candSegs = [];
    pushNonNull(candSegs, tap);
    return candSegs;
  }

  function routeRemoteBusBranch(edge, es, branchPort, spine, routeCtx) {
    const hy = routeCtx.hLaneY[edge.waypoint_row]['e:' + edge._idx];
    const remoteVC = spine.isOut ? es.endVC : es.startVC;
    const remoteKey = (spine.isOut ? 'e_end:' : 'e_start:') + edge._idx;
    const remoteX = routeCtx.vLaneX[remoteVC][remoteKey];
    spine.spineYs.push(hy);
    routeCtx.junctions.push({ x: spine.spineX, y: hy, color: spine.color });

    let hSeg;
    let vSeg;
    let tap;
    if (spine.isOut) {
      hSeg = pushSeg(routeCtx.segments, spine.spineX, hy, remoteX, hy,
        edgeSegMeta(edge, spine.bid, { corridorRow: edge.waypoint_row }));
      vSeg = pushSeg(routeCtx.segments, remoteX, hy, remoteX, branchPort.y,
        edgeSegMeta(edge, spine.bid, { corridorCol: remoteVC }));
      tap = pushSeg(routeCtx.segments, remoteX, branchPort.y, branchPort.x, branchPort.y,
        edgeSegMeta(edge, spine.bid, { isTerminalTo: true, corridorCol: remoteVC }));
    } else {
      tap = pushSeg(routeCtx.segments, branchPort.x, branchPort.y, remoteX, branchPort.y,
        edgeSegMeta(edge, spine.bid, { isTerminalFrom: true, corridorCol: remoteVC }));
      vSeg = pushSeg(routeCtx.segments, remoteX, branchPort.y, remoteX, hy,
        edgeSegMeta(edge, spine.bid, { corridorCol: remoteVC }));
      hSeg = pushSeg(routeCtx.segments, remoteX, hy, spine.spineX, hy,
        edgeSegMeta(edge, spine.bid, { corridorRow: edge.waypoint_row }));
    }
    const candSegs = [];
    pushNonNull(candSegs, hSeg, tap, vSeg);
    return candSegs;
  }

  function routeBusEdge(edge, spine, routeCtx) {
    const es = edgeSides(edge, routeCtx.cards);
    const fc = routeCtx.cards[edge.from];
    const tc = routeCtx.cards[edge.to];
    const branchPort = routeCtx.ports[(spine.isOut ? 'to:' : 'from:') + edge._idx];
    if (!es || !fc || !tc || !branchPort) return;

    const label = formatEdgeLabel(edge);
    const pillW = measurePillWidth(label);
    const candSegs = Math.abs(tc.col - fc.col) <= 1
      ? routeAdjacentBusBranch(edge, branchPort, spine, pillW, routeCtx)
      : routeRemoteBusBranch(edge, es, branchPort, spine, routeCtx);

    if (spine.isOut) {
      routeCtx.arrowHeads.push({ x: branchPort.x, y: branchPort.y, side: branchPort.side, flowKind: edge.flow_kind });
    }
    routeCtx.pillSpecs.push({ edge: edge, label: label, width: pillW, segments: candSegs, color: spine.color });
  }

  function routeBus(bid, bus, es0, color, allInferred, routeCtx) {
    const trunk = buildBusTrunk(bid, bus, es0, allInferred, routeCtx);
    if (!trunk) return;

    const spineYs = [trunk.trunkPort.y];
    const spine = {
      bid: bid,
      isOut: trunk.isOut,
      spineX: trunk.spineX,
      vcIdx: trunk.vcIdx,
      spineYs: spineYs,
      color: color
    };

    if (trunk.isOut) {
      pushSeg(routeCtx.segments, trunk.trunkPort.x, trunk.trunkPort.y, trunk.spineX, trunk.trunkPort.y,
        { isTerminalFrom: true, ...trunk.trunkMeta });
    } else {
      pushSeg(routeCtx.segments, trunk.spineX, trunk.trunkPort.y, trunk.trunkPort.x, trunk.trunkPort.y,
        { isTerminalTo: true, ...trunk.trunkMeta });
      routeCtx.arrowHeads.push({
        x: trunk.trunkPort.x,
        y: trunk.trunkPort.y,
        side: trunk.trunkPort.side,
        flowKind: bus.flowKind
      });
    }

    for (const edge of bus.edges) {
      routeBusEdge(edge, spine, routeCtx);
    }
    finishBusSpine(routeCtx, trunk.spineX, spineYs, trunk.trunkPort.y, trunk.trunkMeta, color);
  }

  function routeAllBuses(busMap, routeCtx) {
    const busIDs = Object.keys(busMap).sort((a, b) => a.localeCompare(b));
    for (const bid of busIDs) {
      const bus = busMap[bid];
      const es0 = edgeSides(bus.edges[0], routeCtx.cards);
      if (!es0) continue;
      const color = FLOW_COLORS[bus.flowKind] || FLOW_COLORS.ADMIN;
      const allInferred = bus.edges.every((e) => e.provenance === 'INFERRED');
      routeBus(bid, bus, es0, color, allInferred, routeCtx);
    }
  }

  function pushThreeStepDetour(routeCtx, edge, pFrom, pTo, lx, corridorCol) {
    const s1 = pushSeg(routeCtx.segments, pFrom.x, pFrom.y, lx, pFrom.y,
      edgeSegMeta(edge, '', { isTerminalFrom: true, corridorCol: corridorCol }));
    const s2 = pushSeg(routeCtx.segments, lx, pFrom.y, lx, pTo.y,
      edgeSegMeta(edge, '', { corridorCol: corridorCol }));
    const s3 = pushSeg(routeCtx.segments, lx, pTo.y, pTo.x, pTo.y,
      edgeSegMeta(edge, '', { isTerminalTo: true, corridorCol: corridorCol }));
    return { s1: s1, s2: s2, s3: s3 };
  }

  function routeStandaloneAdjacent(routeCtx, edge, es, pFrom, pTo, pillW) {
    const candSegs = [];
    if (Math.abs(pFrom.y - pTo.y) < 1) {
      const s = pushSeg(routeCtx.segments, pFrom.x, pFrom.y, pTo.x, pTo.y,
        edgeSegMeta(edge, '', { isTerminalFrom: true, isTerminalTo: true, corridorCol: es.startVC }));
      pushNonNull(candSegs, s);
    } else {
      const lx = routeCtx.vLaneX[es.startVC]['e:' + edge._idx];
      const detour = pushThreeStepDetour(routeCtx, edge, pFrom, pTo, lx, es.startVC);
      pushNonNull(candSegs, detour.s3, detour.s1, detour.s2);
    }
    recordPillNeed(routeCtx.vPillReserve, es.startVC, pillW);
    return candSegs;
  }

  function routeStandaloneMultiCol(routeCtx, edge, es, pFrom, pTo) {
    const sx = routeCtx.vLaneX[es.startVC]['e_start:' + edge._idx];
    const hy = routeCtx.hLaneY[edge.waypoint_row]['e:' + edge._idx];
    const ex = routeCtx.vLaneX[es.endVC]['e_end:' + edge._idx];
    const startTap = pushSeg(routeCtx.segments, pFrom.x, pFrom.y, sx, pFrom.y,
      edgeSegMeta(edge, '', { isTerminalFrom: true, corridorCol: es.startVC }));
    const vStart = pushSeg(routeCtx.segments, sx, pFrom.y, sx, hy,
      edgeSegMeta(edge, '', { corridorCol: es.startVC }));
    const hSeg = pushSeg(routeCtx.segments, sx, hy, ex, hy,
      edgeSegMeta(edge, '', { corridorRow: edge.waypoint_row }));
    const vEnd = pushSeg(routeCtx.segments, ex, hy, ex, pTo.y,
      edgeSegMeta(edge, '', { corridorCol: es.endVC }));
    const tap = pushSeg(routeCtx.segments, ex, pTo.y, pTo.x, pTo.y,
      edgeSegMeta(edge, '', { isTerminalTo: true, corridorCol: es.endVC }));
    const candSegs = [];
    pushNonNull(candSegs, hSeg, tap, startTap, vEnd, vStart);
    return candSegs;
  }

  function routeStandaloneEdge(edge, routeCtx) {
    const fc = routeCtx.cards[edge.from];
    const tc = routeCtx.cards[edge.to];
    const es = edgeSides(edge, routeCtx.cards);
    const pFrom = routeCtx.ports['from:' + edge._idx];
    const pTo = routeCtx.ports['to:' + edge._idx];
    if (!fc || !tc || !es || !pFrom || !pTo) return;

    const color = FLOW_COLORS[edge.flow_kind] || FLOW_COLORS.ADMIN;
    const label = formatEdgeLabel(edge);
    const pillW = measurePillWidth(label);
    let candSegs;
    if (es.dir === 0) {
      // Same-column lateral detour.
      const lx = routeCtx.vLaneX[es.startVC]['e:' + edge._idx];
      const detour = pushThreeStepDetour(routeCtx, edge, pFrom, pTo, lx, es.startVC);
      candSegs = [];
      pushNonNull(candSegs, detour.s2, detour.s3, detour.s1);
      recordPillNeed(routeCtx.vPillReserve, es.startVC, pillW);
    } else if (Math.abs(tc.col - fc.col) === 1) {
      candSegs = routeStandaloneAdjacent(routeCtx, edge, es, pFrom, pTo, pillW);
    } else {
      candSegs = routeStandaloneMultiCol(routeCtx, edge, es, pFrom, pTo);
    }

    routeCtx.arrowHeads.push({ x: pTo.x, y: pTo.y, side: pTo.side, flowKind: edge.flow_kind });
    routeCtx.pillSpecs.push({ edge: edge, label: label, width: pillW, segments: candSegs, color: color });
  }

  function isPillClear(rect, geom, placedPills) {
    for (const card of geom.cardList) {
      if (rectsOverlap(rect, card, CARD_CLEARANCE)) return false;
    }
    for (const badge of geom.badges) {
      if (rectsOverlap(rect, badge, BADGE_CLEARANCE)) return false;
    }
    for (const placed of placedPills) {
      if (rectsOverlap(rect, placed.rect, PILL_CLEARANCE)) return false;
    }
    return true;
  }

  function makePillRect(cx, cy, w, h) {
    return { left: cx - w / 2, top: cy - h / 2, right: cx + w / 2, bottom: cy + h / 2 };
  }

  function collectHorizontalPillCandidates(seg, w, vCorridors, candidates) {
    const minX = Math.min(seg.x1, seg.x2);
    const maxX = Math.max(seg.x1, seg.x2);
    const span = maxX - minX;
    const xs = [(minX + maxX) / 2];
    if (seg.corridorCol !== undefined && vCorridors[seg.corridorCol]) {
      const vc = vCorridors[seg.corridorCol];
      xs.push(vc.cx, vc.left + w / 2 + 6, vc.right - w / 2 - 6);
    }
    if (span > w + 12) {
      const usableMin = minX + w / 2 + 6;
      const usableMax = maxX - w / 2 - 6;
      const usableSpan = usableMax - usableMin;
      for (const f of [0.3, 0.7, 0.15, 0.85, 0.4, 0.6, 0.0, 1.0]) {
        xs.push(usableMin + usableSpan * f);
      }
      for (let x = usableMin; x <= usableMax; x += 24) {
        xs.push(x);
      }
    }
    for (const dy of [0, -12, 12, -22, 22]) {
      for (const cx of xs) {
        candidates.push({ cx: cx, cy: seg.y1 + dy });
      }
    }
  }

  function collectVerticalPillCandidates(seg, vCorridors, candidates) {
    const minY = Math.min(seg.y1, seg.y2);
    const maxY = Math.max(seg.y1, seg.y2);
    const xs = [seg.x1];
    if (seg.corridorCol !== undefined && vCorridors[seg.corridorCol]) {
      xs.push(vCorridors[seg.corridorCol].cx);
    }
    for (const cx of xs) {
      for (const f of [0.5, 0.3, 0.7, 0.18, 0.82]) {
        candidates.push({ cx: cx, cy: minY + (maxY - minY) * f });
      }
    }
  }

  function choosePillPlacement(candidates, w, h, geom, placedPills) {
    for (const c of candidates) {
      const rect = makePillRect(c.cx, c.cy, w, h);
      if (isPillClear(rect, geom, placedPills)) {
        return { cx: c.cx, cy: c.cy, rect: rect };
      }
    }
    const fallback = candidates[0];
    return { cx: fallback.cx, cy: fallback.cy, rect: makePillRect(fallback.cx, fallback.cy, w, h) };
  }

  function placeVerbPills(pillSpecs, geom) {
    const placedPills = [];
    for (const spec of pillSpecs) {
      if (!spec.label || spec.segments.length === 0) continue;
      const candidates = [];
      for (const seg of spec.segments) {
        if (seg.isHorizontal) {
          collectHorizontalPillCandidates(seg, spec.width, geom.vCorridors, candidates);
        } else {
          collectVerticalPillCandidates(seg, geom.vCorridors, candidates);
        }
      }
      const chosen = choosePillPlacement(candidates, spec.width, PILL_HEIGHT, geom, placedPills);
      placedPills.push({
        edge: spec.edge,
        label: spec.label,
        color: spec.color,
        cx: chosen.cx,
        cy: chosen.cy,
        width: spec.width,
        height: PILL_HEIGHT,
        rect: chosen.rect
      });
    }
    return placedPills;
  }

  function checkSegmentCardCollision(viewID, seg, card, collisions) {
    const incident = (card.id === seg.fromID && seg.isTerminalFrom) || (card.id === seg.toID && seg.isTerminalTo);
    if (incident) {
      if (segPenetratesInterior(seg, card)) {
        collisions.push({
          view_id: viewID,
          kind: 'card_segment_collision',
          subjects: [seg.fromID + '->' + seg.toID, card.id],
          detail: 'segment penetrates incident card interior'
        });
      }
    } else if (segIntersectsRect(seg, card, CARD_CLEARANCE)) {
      collisions.push({
        view_id: viewID,
        kind: 'card_segment_collision',
        subjects: [seg.fromID + '->' + seg.toID, card.id],
        detail: 'segment violates 4px clearance around card ' + card.id
      });
    }
  }

  function checkSegmentObstacleCollisions(viewID, segments, geom, collisions) {
    for (const seg of segments) {
      for (const card of geom.cardList) {
        checkSegmentCardCollision(viewID, seg, card, collisions);
      }
      for (const badge of geom.badges) {
        if (segIntersectsRect(seg, badge, BADGE_CLEARANCE)) {
          collisions.push({
            view_id: viewID,
            kind: 'badge_segment_collision',
            subjects: [seg.fromID + '->' + seg.toID, badge.id],
            detail: 'segment violates 4px clearance around container badge ' + badge.id
          });
        }
      }
    }
  }

  function checkSinglePillObstacles(viewID, p, pName, geom, collisions) {
    for (const card of geom.cardList) {
      if (rectsOverlap(p.rect, card, CARD_CLEARANCE)) {
        collisions.push({
          view_id: viewID,
          kind: 'pill_card_collision',
          subjects: [pName, card.id],
          detail: 'verb pill overlaps card ' + card.id
        });
      }
    }
    for (const badge of geom.badges) {
      if (rectsOverlap(p.rect, badge, BADGE_CLEARANCE)) {
        collisions.push({
          view_id: viewID,
          kind: 'pill_badge_collision',
          subjects: [pName, badge.id],
          detail: 'verb pill overlaps container badge ' + badge.id
        });
      }
    }
  }

  function checkPillCollisions(viewID, placedPills, geom, collisions) {
    for (let i = 0; i < placedPills.length; i++) {
      const p = placedPills[i];
      const pName = p.edge.from + '->' + p.edge.to + ':' + p.label;
      checkSinglePillObstacles(viewID, p, pName, geom, collisions);
      for (let j = i + 1; j < placedPills.length; j++) {
        const q = placedPills[j];
        if (rectsOverlap(p.rect, q.rect, PILL_CLEARANCE)) {
          collisions.push({
            view_id: viewID,
            kind: 'pill_pill_collision',
            subjects: [pName, q.edge.from + '->' + q.edge.to + ':' + q.label],
            detail: 'verb pills violate 4px separation'
          });
        }
      }
    }
  }

  function intervalsOverlapBeyond1px(a1, a2, b1, b2) {
    const ovStart = Math.max(Math.min(a1, a2), Math.min(b1, b2));
    const ovEnd = Math.min(Math.max(a1, a2), Math.max(b1, b2));
    return ovEnd - ovStart > 1;
  }

  function checkCollinearPair(viewID, a, b, collisions) {
    const horiz = a.isHorizontal;
    const close = horiz
      ? Math.abs(a.y1 - b.y1) < COLLINEAR_MIN_SEP - 0.1
      : Math.abs(a.x1 - b.x1) < COLLINEAR_MIN_SEP - 0.1;
    if (!close) return;
    const overlaps = horiz
      ? intervalsOverlapBeyond1px(a.x1, a.x2, b.x1, b.x2)
      : intervalsOverlapBeyond1px(a.y1, a.y2, b.y1, b.y2);
    if (!overlaps) return;
    collisions.push({
      view_id: viewID,
      kind: 'collinear_collision',
      subjects: [a.fromID + '->' + a.toID, b.fromID + '->' + b.toID],
      detail: horiz ? 'parallel horizontal segments closer than 6px' : 'parallel vertical segments closer than 6px'
    });
  }

  function recordPerpendicularBridge(a, b, idxA, idxB, bridgesBySegIdx) {
    if (a.busGroup && a.busGroup === b.busGroup) return;
    const hSeg = a.isHorizontal ? a : b;
    const hIdx = a.isHorizontal ? idxA : idxB;
    const vSeg = a.isVertical ? a : b;
    const hMinX = Math.min(hSeg.x1, hSeg.x2);
    const hMaxX = Math.max(hSeg.x1, hSeg.x2);
    const vMinY = Math.min(vSeg.y1, vSeg.y2);
    const vMaxY = Math.max(vSeg.y1, vSeg.y2);
    if (
      vSeg.x1 > hMinX + BRIDGE_RADIUS &&
      vSeg.x1 < hMaxX - BRIDGE_RADIUS &&
      hSeg.y1 > vMinY + BRIDGE_RADIUS &&
      hSeg.y1 < vMaxY - BRIDGE_RADIUS
    ) {
      if (!bridgesBySegIdx[hIdx]) bridgesBySegIdx[hIdx] = [];
      bridgesBySegIdx[hIdx].push(vSeg.x1);
    }
  }

  function checkSegmentPairsAndBridges(viewID, segments, collisions) {
    const bridgesBySegIdx = {};
    for (let i = 0; i < segments.length; i++) {
      const a = segments[i];
      for (let j = i + 1; j < segments.length; j++) {
        const b = segments[j];
        if (a.edgeIdx === b.edgeIdx) continue;
        if (a.isHorizontal === b.isHorizontal) {
          checkCollinearPair(viewID, a, b, collisions);
        } else {
          recordPerpendicularBridge(a, b, i, j, bridgesBySegIdx);
        }
      }
    }
    return bridgesBySegIdx;
  }

  function routeViewPass(viewData, geom) {
    const edges = viewData.edges || [];
    const cards = geom.cards;
    const { busMap, standalone } = groupEdgesByBus(edges);

    // Step 1: Collect port allocations and lane registrations.
    const { portGroups, vLaneGroups, hLaneGroups } = collectPortsAndLanes(busMap, standalone, cards);
    const ports = assignPortCoords(portGroups, cards);

    // Step 2: Assign discrete 8px lane coordinates inside each corridor.
    const { vLaneX, vLaneCounts } = assignVerticalLanes(vLaneGroups, geom.vCorridors);
    const { hLaneY, hLaneCounts, hBadgeReserve } = assignHorizontalLanes(hLaneGroups, geom.hCorridors, geom.badges);

    const routeCtx = {
      cards: cards,
      geom: geom,
      ports: ports,
      vLaneX: vLaneX,
      vLaneCounts: vLaneCounts,
      vPillReserve: {},
      hLaneY: hLaneY,
      segments: [],
      junctions: [],
      arrowHeads: [],
      pillSpecs: []
    };

    // Step 3: Build orthogonal segments for shared bus spines.
    routeAllBuses(busMap, routeCtx);

    // Step 4: Build orthogonal segments for standalone edges.
    for (const edge of standalone) {
      routeStandaloneEdge(edge, routeCtx);
    }

    // Step 5: Place <= 16-char verb pills in collision-free spans.
    const placedPills = placeVerbPills(routeCtx.pillSpecs, geom);

    // Step 6: Strict Collision & Crossing Verification.
    const collisions = [];
    checkSegmentObstacleCollisions(viewData.id, routeCtx.segments, geom, collisions);
    checkPillCollisions(viewData.id, placedPills, geom, collisions);
    const bridgesBySegIdx = checkSegmentPairsAndBridges(viewData.id, routeCtx.segments, collisions);

    return {
      segments: routeCtx.segments,
      junctions: routeCtx.junctions,
      arrowHeads: routeCtx.arrowHeads,
      placedPills: placedPills,
      bridgesBySegIdx: bridgesBySegIdx,
      collisions: collisions,
      vLaneCounts: vLaneCounts,
      vPillReserve: routeCtx.vPillReserve,
      hLaneCounts: hLaneCounts,
      hBadgeReserve: hBadgeReserve
    };
  }

  function applyCorridorWidening(sheet, geom, passRes) {
    let widened = false;
    Object.keys(geom.vCorridors).forEach(function (cKey) {
      const cIdx = Number.parseInt(cKey, 10);
      const vc = geom.vCorridors[cIdx];
      const lanes = passRes.vLaneCounts[cIdx] || 0;
      const pillRes = passRes.vPillReserve[cIdx] || 0;
      if (lanes === 0 && pillRes === 0) return;
      const laneNeed = 24 + LANE_STEP * lanes;
      const needed = Math.min(MAX_CORRIDOR_SIZE, Math.max(laneNeed, pillRes));
      if (needed > vc.width + 1) {
        sheet.style.setProperty('--corridor-col-' + cIdx, Math.ceil(needed) + 'px');
        widened = true;
      }
    });

    Object.keys(geom.hCorridors).forEach(function (rKey) {
      const rIdx = Number.parseInt(rKey, 10);
      const hc = geom.hCorridors[rIdx];
      const lanes = passRes.hLaneCounts[rIdx] || 0;
      const badgeRes = passRes.hBadgeReserve[rIdx] || 0;
      if (lanes === 0) return;
      const needed = Math.min(MAX_CORRIDOR_SIZE, 24 + LANE_STEP * lanes + badgeRes + 12);
      if (needed > hc.height + 1) {
        sheet.style.setProperty('--corridor-row-' + rIdx, Math.ceil(needed) + 'px');
        widened = true;
      }
    });

    // If collisions remain, push any corridor below cap up to MAX_CORRIDOR_SIZE.
    if (!widened && passRes.collisions.length > 0) {
      Object.keys(geom.vCorridors).forEach(function (cKey) {
        const cIdx = Number.parseInt(cKey, 10);
        const vc = geom.vCorridors[cIdx];
        if ((passRes.vLaneCounts[cIdx] || 0) > 0 && vc.width < MAX_CORRIDOR_SIZE - 1) {
          sheet.style.setProperty('--corridor-col-' + cIdx, MAX_CORRIDOR_SIZE + 'px');
          widened = true;
        }
      });
      Object.keys(geom.hCorridors).forEach(function (rKey) {
        const rIdx = Number.parseInt(rKey, 10);
        const hc = geom.hCorridors[rIdx];
        if ((passRes.hLaneCounts[rIdx] || 0) > 0 && hc.height < MAX_CORRIDOR_SIZE - 1) {
          sheet.style.setProperty('--corridor-row-' + rIdx, MAX_CORRIDOR_SIZE + 'px');
          widened = true;
        }
      });
    }

    if (widened) {
      sheet.getBoundingClientRect();
    }
    return widened;
  }

  function svgEl(tag, attrs) {
    const el = document.createElementNS(SVG_NS, tag);
    Object.keys(attrs).forEach(function (k) {
      el.setAttribute(k, String(attrs[k]));
    });
    return el;
  }

  function buildHorizontalBridgePath(seg, bridgeXs) {
    const y = seg.y1;
    const goRight = seg.x2 >= seg.x1;
    const sorted = bridgeXs.slice().sort(function (a, b) {
      return goRight ? (a - b) : (b - a);
    });
    const parts = ['M', seg.x1.toFixed(1), y.toFixed(1)];
    sorted.forEach(function (bx) {
      const startX = goRight ? bx - BRIDGE_RADIUS : bx + BRIDGE_RADIUS;
      const endX = goRight ? bx + BRIDGE_RADIUS : bx - BRIDGE_RADIUS;
      const sweep = goRight ? 1 : 0;
      parts.push(
        'L', startX.toFixed(1), y.toFixed(1),
        'A', BRIDGE_RADIUS, BRIDGE_RADIUS, 0, 0, sweep, endX.toFixed(1), y.toFixed(1)
      );
    });
    parts.push('L', seg.x2.toFixed(1), y.toFixed(1));
    return parts.join(' ');
  }

  function injectConnectorSVG(sheet, geom, passRes) {
    const existing = sheet.querySelector('svg.connector-layer');
    if (existing) existing.remove();

    const svg = svgEl('svg', {
      class: 'connector-layer',
      viewBox: '0 0 ' + geom.width.toFixed(1) + ' ' + geom.height.toFixed(1),
      width: geom.width.toFixed(1),
      height: geom.height.toFixed(1)
    });

    // Pass 1: Draw all non-bridged segments (including all vertical segments) underneath bridges.
    const bridgedIndices = [];
    passRes.segments.forEach(function (seg, idx) {
      const bridges = passRes.bridgesBySegIdx[idx];
      if (seg.isHorizontal && bridges?.length > 0) {
        bridgedIndices.push(idx);
        return;
      }
      const color = FLOW_COLORS[seg.flowKind] || FLOW_COLORS.ADMIN;
      const attrs = {
        x1: seg.x1.toFixed(1),
        y1: seg.y1.toFixed(1),
        x2: seg.x2.toFixed(1),
        y2: seg.y2.toFixed(1),
        stroke: color,
        'stroke-width': '1.5',
        'stroke-linecap': 'round'
      };
      if (seg.inferred) attrs['stroke-dasharray'] = '5 3';
      svg.appendChild(svgEl('line', attrs));
    });

    // Pass 2: Draw bridged horizontal segments on top so 4px white halos cleanly mask vertical crossings.
    bridgedIndices.forEach(function (idx) {
      const seg = passRes.segments[idx];
      const color = FLOW_COLORS[seg.flowKind] || FLOW_COLORS.ADMIN;
      const d = buildHorizontalBridgePath(seg, passRes.bridgesBySegIdx[idx]);
      svg.appendChild(svgEl('path', {
        d: d,
        fill: 'none',
        stroke: '#FFFFFF',
        'stroke-width': '5.5',
        'stroke-linecap': 'round',
        'data-bridge-halo': '1'
      }));
      const pathAttrs = {
        d: d,
        fill: 'none',
        stroke: color,
        'stroke-width': '1.5',
        'stroke-linecap': 'round',
        'data-bridge': '1'
      };
      if (seg.inferred) pathAttrs['stroke-dasharray'] = '5 3';
      svg.appendChild(svgEl('path', pathAttrs));
    });

    // Draw 2.5px bus junction dots.
    passRes.junctions.forEach(function (j) {
      svg.appendChild(svgEl('circle', {
        cx: j.x.toFixed(1),
        cy: j.y.toFixed(1),
        r: JUNCTION_RADIUS,
        fill: j.color
      }));
    });

    // Draw crisp arrowheads.
    passRes.arrowHeads.forEach(function (ah) {
      const color = FLOW_COLORS[ah.flowKind] || FLOW_COLORS.ADMIN;
      let pts;
      if (ah.side === 'left') {
        pts = [
          ah.x.toFixed(1) + ',' + ah.y.toFixed(1),
          (ah.x - 6).toFixed(1) + ',' + (ah.y - 3.2).toFixed(1),
          (ah.x - 6).toFixed(1) + ',' + (ah.y + 3.2).toFixed(1)
        ].join(' ');
      } else {
        pts = [
          ah.x.toFixed(1) + ',' + ah.y.toFixed(1),
          (ah.x + 6).toFixed(1) + ',' + (ah.y - 3.2).toFixed(1),
          (ah.x + 6).toFixed(1) + ',' + (ah.y + 3.2).toFixed(1)
        ].join(' ');
      }
      svg.appendChild(svgEl('polygon', { points: pts, fill: color }));
    });

    // Draw verb pills.
    passRes.placedPills.forEach(function (p) {
      const g = svgEl('g', { class: 'verb-pill' });
      g.appendChild(svgEl('rect', {
        x: p.rect.left.toFixed(1),
        y: p.rect.top.toFixed(1),
        width: p.width.toFixed(1),
        height: p.height.toFixed(1),
        rx: '3',
        fill: '#FFFFFF',
        stroke: p.color,
        'stroke-width': '0.75'
      }));
      const txt = svgEl('text', {
        x: p.cx.toFixed(1),
        y: (p.cy + 3.0).toFixed(1),
        'text-anchor': 'middle',
        'font-family': 'Inter, sans-serif',
        'font-size': String(PILL_FONT_SIZE),
        'font-weight': '500',
        fill: p.color
      });
      txt.textContent = p.label;
      g.appendChild(txt);
      svg.appendChild(g);
    });

    sheet.appendChild(svg);
  }

  async function runRouter() {
    if (document.documentElement.dataset.gcpArchRouted === '1') {
      const res = {
        ok: true,
        collisions: [],
        viewRects: measureViewRects(),
        html: '<!DOCTYPE html>\n' + document.documentElement.outerHTML
      };
      window.__GCP_ARCH_RENDER_RESULT__ = res;
      return res;
    }

    if (document.fonts) {
      await document.fonts.ready;
    }
    await new Promise(function (resolve) {
      requestAnimationFrame(function () {
        requestAnimationFrame(resolve);
      });
    });

    const dataEl = document.getElementById('gcp-arch-routing-data');
    const routingViews = dataEl ? JSON.parse(dataEl.textContent) : [];
    const sheets = document.querySelectorAll('.view-sheet');

    const allCollisions = [];
    const finalDrawings = [];

    sheets.forEach(function (sheet, idx) {
      const viewData = routingViews[idx] || { id: sheet.dataset.viewId || ('view_' + idx), edges: [] };
      let geom = null;
      let passRes = null;

      for (let pass = 0; pass <= MAX_REFLOW_PASSES; pass++) {
        geom = measureSheetGeometry(sheet);
        passRes = routeViewPass(viewData, geom);
        const widened = pass < MAX_REFLOW_PASSES && applyCorridorWidening(sheet, geom, passRes);
        if (!widened) {
          break;
        }
      }

      if (passRes.collisions.length > 0) {
        for (const col of passRes.collisions) {
          allCollisions.push(col);
        }
      } else {
        finalDrawings.push({ sheet: sheet, geom: geom, passRes: passRes });
      }
    });

    if (allCollisions.length > 0) {
      const failRes = {
        ok: false,
        collisions: allCollisions,
        viewRects: [],
        html: ''
      };
      window.__GCP_ARCH_RENDER_RESULT__ = failRes;
      return failRes;
    }

    finalDrawings.forEach(function (d) {
      injectConnectorSVG(d.sheet, d.geom, d.passRes);
      const h = d.sheet.getBoundingClientRect().height;
      if (h > 708) {
        d.sheet.style.setProperty('--print-scale', (708 / h).toFixed(4));
      }
    });

    document.documentElement.dataset.gcpArchRouted = '1';
    const okRes = {
      ok: true,
      collisions: [],
      viewRects: measureViewRects(),
      html: '<!DOCTYPE html>\n' + document.documentElement.outerHTML
    };
    window.__GCP_ARCH_RENDER_RESULT__ = okRes;
    return okRes;
  }

  window.__GCP_ARCH_RENDER_PROMISE__ = runRouter();
})();
