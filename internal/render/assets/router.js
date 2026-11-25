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
      box.col = parseInt(el.dataset.gridCol || '0', 10);
      box.row = parseInt(el.dataset.gridRow || '0', 10);
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
      const idx = parseInt(el.dataset.corridorCol || '0', 10);
      const box = rel(el);
      box.index = idx;
      vCorridors[idx] = box;
    });

    const hCorridors = {};
    sheet.querySelectorAll('.corridor-row').forEach(function (el) {
      const idx = parseInt(el.dataset.corridorRow || '0', 10);
      const box = rel(el);
      box.index = idx;
      hCorridors[idx] = box;
    });

    const waypoints = {};
    sheet.querySelectorAll('.edge-waypoint').forEach(function (el) {
      const eIdx = parseInt(el.dataset.edgeIdx || '-1', 10);
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

  function assignPortCoords(portGroups, cards) {
    const resolved = {};
    Object.keys(portGroups).forEach(function (key) {
      const items = portGroups[key];
      const parts = key.split('|');
      const cardID = parts[0];
      const side = parts[1];
      const card = cards[cardID];
      if (!card) return;

      items.sort(function (a, b) {
        if (Math.abs(a.sortY - b.sortY) > 0.5) return a.sortY - b.sortY;
        return a.order - b.order;
      });

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
    });

    // De-conflict opposing-side ports inside the same vertical corridor so horizontal trunks/taps
    // of distinct edges/buses never share a Y within COLLINEAR_MIN_SEP (6px).
    const byCorridor = {};
    Object.keys(resolved).forEach(function (pk) {
      const p = resolved[pk];
      const vc = p.side === 'right' ? p.col : (p.col - 1);
      if (vc < 0) return;
      if (!byCorridor[vc]) byCorridor[vc] = { left: [], right: [] };
      if (p.side === 'right') {
        byCorridor[vc].left.push(p);
      } else {
        byCorridor[vc].right.push(p);
      }
    });

    const minSep = COLLINEAR_MIN_SEP + 2; // 8px separation
    Object.keys(byCorridor).forEach(function (vcKey) {
      const leftPorts = byCorridor[vcKey].left;
      const rightPorts = byCorridor[vcKey].right;
      for (let pass = 0; pass < 3; pass++) {
        for (let i = 0; i < rightPorts.length; i++) {
          const rp = rightPorts[i];
          for (let j = 0; j < leftPorts.length; j++) {
            const lp = leftPorts[j];
            if (rp.groupTag === lp.groupTag) continue;
            const diff = rp.y - lp.y;
            if (Math.abs(diff) < minSep - 0.1) {
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
          }
        }
      }
    });

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

  function clearHorizontalBand(hc, badges, minX, maxX) {
    let top = hc.top + CORRIDOR_WALL_PAD;
    let bottom = hc.bottom - CORRIDOR_WALL_PAD;
    if (bottom < top) {
      top = hc.cy;
      bottom = hc.cy;
    }
    const blocked = [];
    for (let i = 0; i < badges.length; i++) {
      const b = badges[i];
      if (b.right + BADGE_CLEARANCE > minX && b.left - BADGE_CLEARANCE < maxX) {
        if (b.bottom + BADGE_CLEARANCE > hc.top && b.top - BADGE_CLEARANCE < hc.bottom) {
          blocked.push({ top: b.top - BADGE_CLEARANCE - 1, bottom: b.bottom + BADGE_CLEARANCE + 1 });
        }
      }
    }
    if (blocked.length === 0) {
      return { top: top, bottom: bottom, badgeHeight: 0 };
    }
    blocked.sort(function (a, b) { return a.top - b.top; });
    const intervals = [];
    let cursor = hc.top + 6;
    const limit = hc.bottom - 6;
    let maxBadgeH = 0;
    for (let i = 0; i < blocked.length; i++) {
      maxBadgeH = Math.max(maxBadgeH, blocked[i].bottom - blocked[i].top);
      if (blocked[i].top > cursor) {
        intervals.push({ top: cursor, bottom: Math.min(limit, blocked[i].top) });
      }
      cursor = Math.max(cursor, blocked[i].bottom);
    }
    if (cursor < limit) {
      intervals.push({ top: cursor, bottom: limit });
    }
    if (intervals.length === 0) {
      return { top: top, bottom: bottom, badgeHeight: maxBadgeH };
    }
    intervals.sort(function (a, b) { return (b.bottom - b.top) - (a.bottom - a.top); });
    return { top: intervals[0].top, bottom: intervals[0].bottom, badgeHeight: maxBadgeH };
  }

  function routeViewPass(viewData, geom) {
    const edges = viewData.edges || [];
    const cards = geom.cards;
    const segments = [];
    const junctions = [];
    const arrowHeads = [];
    const pillSpecs = [];
    const vLaneGroups = {};
    const hLaneGroups = {};

    function registerVLane(corridorIdx, key, sortY, order) {
      if (!vLaneGroups[corridorIdx]) vLaneGroups[corridorIdx] = {};
      if (!vLaneGroups[corridorIdx][key]) {
        vLaneGroups[corridorIdx][key] = { key: key, sortY: sortY, order: order };
      }
    }

    function registerHLane(corridorIdx, key, sortX, order, minX, maxX) {
      if (!hLaneGroups[corridorIdx]) hLaneGroups[corridorIdx] = {};
      if (!hLaneGroups[corridorIdx][key]) {
        hLaneGroups[corridorIdx][key] = { key: key, sortX: sortX, order: order, minX: minX, maxX: maxX };
      } else {
        hLaneGroups[corridorIdx][key].minX = Math.min(hLaneGroups[corridorIdx][key].minX, minX);
        hLaneGroups[corridorIdx][key].maxX = Math.max(hLaneGroups[corridorIdx][key].maxX, maxX);
      }
    }

    // Group edges into buses vs standalone edges.
    const busMap = {};
    const standalone = [];
    edges.forEach(function (edge, idx) {
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
    });

    // Step 1: Collect port allocations.
    const portGroups = {};
    function addPort(cardID, side, portKey, sortY, order, groupTag) {
      const gk = cardID + '|' + side;
      if (!portGroups[gk]) portGroups[gk] = [];
      portGroups[gk].push({ portKey: portKey, sortY: sortY, order: order, groupTag: groupTag || portKey });
    }

    function edgeSides(edge) {
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

    Object.keys(busMap).sort().forEach(function (bid) {
      const bus = busMap[bid];
      if (bus.kind === 'out') {
        const first = bus.edges[0];
        const s0 = edgeSides(first);
        if (!s0) return;
        let avgY = 0;
        bus.edges.forEach(function (e) {
          const tc = cards[e.to];
          avgY += tc ? tc.cy : 0;
          const es = edgeSides(e);
          if (es && tc) {
            addPort(e.to, es.toSide, 'to:' + e._idx, cards[e.from].cy, e._idx, 'bus:' + bid);
          }
        });
        avgY /= bus.edges.length;
        addPort(first.from, s0.fromSide, 'bus:' + bid, avgY, first._idx, 'bus:' + bid);
        registerVLane(s0.startVC, 'bus:' + bid, avgY, first._idx);
        bus.edges.forEach(function (e) {
          const es = edgeSides(e);
          const fc = cards[e.from];
          const tc = cards[e.to];
          if (!es || !fc || !tc) return;
          if (Math.abs(tc.col - fc.col) > 1) {
            registerHLane(e.waypoint_row, 'e:' + e._idx, Math.min(fc.cx, tc.cx), e._idx, Math.min(fc.right, tc.right), Math.max(fc.left, tc.left));
            registerVLane(es.endVC, 'e_end:' + e._idx, tc.cy, e._idx);
          }
        });
      } else {
        const first = bus.edges[0];
        const s0 = edgeSides(first);
        if (!s0) return;
        let avgY = 0;
        bus.edges.forEach(function (e) {
          const fc = cards[e.from];
          avgY += fc ? fc.cy : 0;
          const es = edgeSides(e);
          if (es && fc) {
            addPort(e.from, es.fromSide, 'from:' + e._idx, cards[e.to].cy, e._idx, 'bus:' + bid);
          }
        });
        avgY /= bus.edges.length;
        addPort(first.to, s0.toSide, 'bus:' + bid, avgY, first._idx, 'bus:' + bid);
        registerVLane(s0.endVC, 'bus:' + bid, avgY, first._idx);
        bus.edges.forEach(function (e) {
          const es = edgeSides(e);
          const fc = cards[e.from];
          const tc = cards[e.to];
          if (!es || !fc || !tc) return;
          if (Math.abs(tc.col - fc.col) > 1) {
            registerVLane(es.startVC, 'e_start:' + e._idx, fc.cy, e._idx);
            registerHLane(e.waypoint_row, 'e:' + e._idx, Math.min(fc.cx, tc.cx), e._idx, Math.min(fc.right, tc.right), Math.max(fc.left, tc.left));
          }
        });
      }
    });

    standalone.forEach(function (edge) {
      const fc = cards[edge.from];
      const tc = cards[edge.to];
      const es = edgeSides(edge);
      if (!fc || !tc || !es) return;
      addPort(edge.from, es.fromSide, 'from:' + edge._idx, tc.cy, edge._idx, 'edge:' + edge._idx);
      addPort(edge.to, es.toSide, 'to:' + edge._idx, fc.cy, edge._idx, 'edge:' + edge._idx);
      if (es.dir === 0) {
        registerVLane(es.startVC, 'e:' + edge._idx, (fc.cy + tc.cy) / 2, edge._idx);
      } else if (Math.abs(tc.col - fc.col) === 1) {
        registerVLane(es.startVC, 'e:' + edge._idx, (fc.cy + tc.cy) / 2, edge._idx);
      } else {
        registerVLane(es.startVC, 'e_start:' + edge._idx, fc.cy, edge._idx);
        registerHLane(edge.waypoint_row, 'e:' + edge._idx, Math.min(fc.cx, tc.cx), edge._idx, Math.min(fc.right, tc.right), Math.max(fc.left, tc.left));
        registerVLane(es.endVC, 'e_end:' + edge._idx, tc.cy, edge._idx);
      }
    });

    const ports = assignPortCoords(portGroups, cards);

    // Step 2: Assign discrete 8px lane coordinates inside each corridor.
    const vLaneX = {};
    const vLaneCounts = {};
    const vPillReserve = {};
    Object.keys(vLaneGroups).forEach(function (cKey) {
      const cIdx = parseInt(cKey, 10);
      const vc = geom.vCorridors[cIdx];
      if (!vc) return;
      const items = Object.keys(vLaneGroups[cKey]).map(function (k) { return vLaneGroups[cKey][k]; });
      items.sort(function (a, b) {
        if (Math.abs(a.sortY - b.sortY) > 0.5) return a.sortY - b.sortY;
        return a.order - b.order;
      });
      vLaneCounts[cIdx] = items.length;
      const leftBound = vc.left + CORRIDOR_WALL_PAD;
      const rightBound = vc.right - CORRIDOR_WALL_PAD;
      vLaneX[cIdx] = {};
      for (let i = 0; i < items.length; i++) {
        vLaneX[cIdx][items[i].key] = lanePosition(vc.cx, leftBound, rightBound, i, items.length);
      }
    });

    const hLaneY = {};
    const hLaneCounts = {};
    const hBadgeReserve = {};
    Object.keys(hLaneGroups).forEach(function (rKey) {
      const rIdx = parseInt(rKey, 10);
      const hc = geom.hCorridors[rIdx];
      if (!hc) return;
      const items = Object.keys(hLaneGroups[rKey]).map(function (k) { return hLaneGroups[rKey][k]; });
      items.sort(function (a, b) {
        if (Math.abs(a.sortX - b.sortX) > 0.5) return a.sortX - b.sortX;
        return a.order - b.order;
      });
      hLaneCounts[rIdx] = items.length;
      let minX = Infinity, maxX = -Infinity;
      items.forEach(function (it) {
        minX = Math.min(minX, it.minX);
        maxX = Math.max(maxX, it.maxX);
      });
      const band = clearHorizontalBand(hc, geom.badges, minX, maxX);
      hBadgeReserve[rIdx] = band.badgeHeight;
      hLaneY[rIdx] = {};
      const centerY = (band.top + band.bottom) / 2;
      for (let i = 0; i < items.length; i++) {
        hLaneY[rIdx][items[i].key] = lanePosition(centerY, band.top, band.bottom, i, items.length);
      }
    });

    function pushSeg(x1, y1, x2, y2, meta) {
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

    function recordPillNeed(corridorCol, labelW) {
      if (corridorCol === undefined || corridorCol === null) return;
      vPillReserve[corridorCol] = Math.max(vPillReserve[corridorCol] || 0, labelW + CARD_CLEARANCE * 2 + 2);
    }

    // Step 3: Build orthogonal segments for shared bus spines.
    Object.keys(busMap).sort().forEach(function (bid) {
      const bus = busMap[bid];
      const first = bus.edges[0];
      const es0 = edgeSides(first);
      if (!es0) return;
      const color = FLOW_COLORS[bus.flowKind] || FLOW_COLORS.ADMIN;
      const allInferred = bus.edges.every(function (e) { return e.provenance === 'INFERRED'; });

      if (bus.kind === 'out') {
        const pFrom = ports['bus:' + bid];
        const vcIdx = es0.startVC;
        const vc = geom.vCorridors[vcIdx];
        if (!pFrom || !vc) return;
        // Bias single-spine bus slightly toward source side to open horizontal tap space for verb pills.
        let spineX = vLaneX[vcIdx]['bus:' + bid];
        if (vLaneCounts[vcIdx] === 1 && vc.width >= 72) {
          spineX = es0.dir >= 0 ? (vc.left + CORRIDOR_WALL_PAD + 4) : (vc.right - CORRIDOR_WALL_PAD - 4);
        }

        const spineYs = [pFrom.y];
        pushSeg(pFrom.x, pFrom.y, spineX, pFrom.y, {
          edgeIdx: first._idx, busGroup: bid, fromID: first.from, toID: '', flowKind: bus.flowKind,
          inferred: allInferred, isTerminalFrom: true, corridorCol: vcIdx
        });

        bus.edges.forEach(function (edge) {
          const es = edgeSides(edge);
          const fc = cards[edge.from];
          const tc = cards[edge.to];
          const pTo = ports['to:' + edge._idx];
          if (!es || !fc || !tc || !pTo) return;
          const label = formatEdgeLabel(edge);
          const pillW = measurePillWidth(label);
          const inferred = edge.provenance === 'INFERRED';
          const candSegs = [];

          if (Math.abs(tc.col - fc.col) <= 1) {
            spineYs.push(pTo.y);
            const tap = pushSeg(spineX, pTo.y, pTo.x, pTo.y, {
              edgeIdx: edge._idx, busGroup: bid, fromID: edge.from, toID: edge.to,
              flowKind: edge.flow_kind, inferred: inferred, isTerminalTo: true, corridorCol: vcIdx
            });
            if (tap) candSegs.push(tap);
            recordPillNeed(vcIdx, pillW);
            junctions.push({ x: spineX, y: pTo.y, color: color });
          } else {
            const hy = hLaneY[edge.waypoint_row]['e:' + edge._idx];
            const endX = vLaneX[es.endVC]['e_end:' + edge._idx];
            spineYs.push(hy);
            junctions.push({ x: spineX, y: hy, color: color });
            const hSeg = pushSeg(spineX, hy, endX, hy, {
              edgeIdx: edge._idx, busGroup: bid, fromID: edge.from, toID: edge.to,
              flowKind: edge.flow_kind, inferred: inferred, corridorRow: edge.waypoint_row
            });
            const vSeg = pushSeg(endX, hy, endX, pTo.y, {
              edgeIdx: edge._idx, busGroup: bid, fromID: edge.from, toID: edge.to,
              flowKind: edge.flow_kind, inferred: inferred, corridorCol: es.endVC
            });
            const tap = pushSeg(endX, pTo.y, pTo.x, pTo.y, {
              edgeIdx: edge._idx, busGroup: bid, fromID: edge.from, toID: edge.to,
              flowKind: edge.flow_kind, inferred: inferred, isTerminalTo: true, corridorCol: es.endVC
            });
            if (hSeg) candSegs.push(hSeg);
            if (tap) candSegs.push(tap);
            if (vSeg) candSegs.push(vSeg);
          }

          arrowHeads.push({ x: pTo.x, y: pTo.y, side: pTo.side, flowKind: edge.flow_kind });
          pillSpecs.push({ edge: edge, label: label, width: pillW, segments: candSegs, color: color });
        });

        const minY = Math.min.apply(null, spineYs);
        const maxY = Math.max.apply(null, spineYs);
        if (maxY - minY > 0.5) {
          pushSeg(spineX, minY, spineX, maxY, {
            edgeIdx: first._idx, busGroup: bid, fromID: first.from, toID: '', flowKind: bus.flowKind,
            inferred: allInferred, corridorCol: vcIdx
          });
          if (pFrom.y > minY + 1 && pFrom.y < maxY - 1) {
            junctions.push({ x: spineX, y: pFrom.y, color: color });
          }
        }
      } else {
        // Fan-in bus sharing one target.
        const pTo = ports['bus:' + bid];
        const vcIdx = es0.endVC;
        const vc = geom.vCorridors[vcIdx];
        if (!pTo || !vc) return;
        let spineX = vLaneX[vcIdx]['bus:' + bid];
        if (vLaneCounts[vcIdx] === 1 && vc.width >= 72) {
          spineX = es0.dir >= 0 ? (vc.right - CORRIDOR_WALL_PAD - 4) : (vc.left + CORRIDOR_WALL_PAD + 4);
        }

        const spineYs = [pTo.y];
        pushSeg(spineX, pTo.y, pTo.x, pTo.y, {
          edgeIdx: first._idx, busGroup: bid, fromID: '', toID: first.to, flowKind: bus.flowKind,
          inferred: allInferred, isTerminalTo: true, corridorCol: vcIdx
        });
        arrowHeads.push({ x: pTo.x, y: pTo.y, side: pTo.side, flowKind: bus.flowKind });

        bus.edges.forEach(function (edge) {
          const es = edgeSides(edge);
          const fc = cards[edge.from];
          const tc = cards[edge.to];
          const pFrom = ports['from:' + edge._idx];
          if (!es || !fc || !tc || !pFrom) return;
          const label = formatEdgeLabel(edge);
          const pillW = measurePillWidth(label);
          const inferred = edge.provenance === 'INFERRED';
          const candSegs = [];

          if (Math.abs(tc.col - fc.col) <= 1) {
            spineYs.push(pFrom.y);
            const tap = pushSeg(pFrom.x, pFrom.y, spineX, pFrom.y, {
              edgeIdx: edge._idx, busGroup: bid, fromID: edge.from, toID: edge.to,
              flowKind: edge.flow_kind, inferred: inferred, isTerminalFrom: true, corridorCol: vcIdx
            });
            if (tap) candSegs.push(tap);
            recordPillNeed(vcIdx, pillW);
            junctions.push({ x: spineX, y: pFrom.y, color: color });
          } else {
            const startX = vLaneX[es.startVC]['e_start:' + edge._idx];
            const hy = hLaneY[edge.waypoint_row]['e:' + edge._idx];
            spineYs.push(hy);
            junctions.push({ x: spineX, y: hy, color: color });
            const startTap = pushSeg(pFrom.x, pFrom.y, startX, pFrom.y, {
              edgeIdx: edge._idx, busGroup: bid, fromID: edge.from, toID: edge.to,
              flowKind: edge.flow_kind, inferred: inferred, isTerminalFrom: true, corridorCol: es.startVC
            });
            const vSeg = pushSeg(startX, pFrom.y, startX, hy, {
              edgeIdx: edge._idx, busGroup: bid, fromID: edge.from, toID: edge.to,
              flowKind: edge.flow_kind, inferred: inferred, corridorCol: es.startVC
            });
            const hSeg = pushSeg(startX, hy, spineX, hy, {
              edgeIdx: edge._idx, busGroup: bid, fromID: edge.from, toID: edge.to,
              flowKind: edge.flow_kind, inferred: inferred, corridorRow: edge.waypoint_row
            });
            if (hSeg) candSegs.push(hSeg);
            if (startTap) candSegs.push(startTap);
            if (vSeg) candSegs.push(vSeg);
          }

          pillSpecs.push({ edge: edge, label: label, width: pillW, segments: candSegs, color: color });
        });

        const minY = Math.min.apply(null, spineYs);
        const maxY = Math.max.apply(null, spineYs);
        if (maxY - minY > 0.5) {
          pushSeg(spineX, minY, spineX, maxY, {
            edgeIdx: first._idx, busGroup: bid, fromID: '', toID: first.to, flowKind: bus.flowKind,
            inferred: allInferred, corridorCol: vcIdx
          });
          if (pTo.y > minY + 1 && pTo.y < maxY - 1) {
            junctions.push({ x: spineX, y: pTo.y, color: color });
          }
        }
      }
    });

    // Step 4: Build orthogonal segments for standalone edges.
    standalone.forEach(function (edge) {
      const fc = cards[edge.from];
      const tc = cards[edge.to];
      const es = edgeSides(edge);
      const pFrom = ports['from:' + edge._idx];
      const pTo = ports['to:' + edge._idx];
      if (!fc || !tc || !es || !pFrom || !pTo) return;
      const color = FLOW_COLORS[edge.flow_kind] || FLOW_COLORS.ADMIN;
      const inferred = edge.provenance === 'INFERRED';
      const label = formatEdgeLabel(edge);
      const pillW = measurePillWidth(label);
      const candSegs = [];

      if (es.dir === 0) {
        // Same-column lateral detour.
        const lx = vLaneX[es.startVC]['e:' + edge._idx];
        const s1 = pushSeg(pFrom.x, pFrom.y, lx, pFrom.y, {
          edgeIdx: edge._idx, fromID: edge.from, toID: edge.to, flowKind: edge.flow_kind,
          inferred: inferred, isTerminalFrom: true, corridorCol: es.startVC
        });
        const s2 = pushSeg(lx, pFrom.y, lx, pTo.y, {
          edgeIdx: edge._idx, fromID: edge.from, toID: edge.to, flowKind: edge.flow_kind,
          inferred: inferred, corridorCol: es.startVC
        });
        const s3 = pushSeg(lx, pTo.y, pTo.x, pTo.y, {
          edgeIdx: edge._idx, fromID: edge.from, toID: edge.to, flowKind: edge.flow_kind,
          inferred: inferred, isTerminalTo: true, corridorCol: es.startVC
        });
        if (s2) candSegs.push(s2);
        if (s3) candSegs.push(s3);
        if (s1) candSegs.push(s1);
        recordPillNeed(es.startVC, pillW);
      } else if (Math.abs(tc.col - fc.col) === 1) {
        const lx = vLaneX[es.startVC]['e:' + edge._idx];
        if (Math.abs(pFrom.y - pTo.y) < 1) {
          const s = pushSeg(pFrom.x, pFrom.y, pTo.x, pFrom.y, {
            edgeIdx: edge._idx, fromID: edge.from, toID: edge.to, flowKind: edge.flow_kind,
            inferred: inferred, isTerminalFrom: true, isTerminalTo: true, corridorCol: es.startVC
          });
          if (s) candSegs.push(s);
        } else {
          const s1 = pushSeg(pFrom.x, pFrom.y, lx, pFrom.y, {
            edgeIdx: edge._idx, fromID: edge.from, toID: edge.to, flowKind: edge.flow_kind,
            inferred: inferred, isTerminalFrom: true, corridorCol: es.startVC
          });
          const s2 = pushSeg(lx, pFrom.y, lx, pTo.y, {
            edgeIdx: edge._idx, fromID: edge.from, toID: edge.to, flowKind: edge.flow_kind,
            inferred: inferred, corridorCol: es.startVC
          });
          const s3 = pushSeg(lx, pTo.y, pTo.x, pTo.y, {
            edgeIdx: edge._idx, fromID: edge.from, toID: edge.to, flowKind: edge.flow_kind,
            inferred: inferred, isTerminalTo: true, corridorCol: es.startVC
          });
          if (s3) candSegs.push(s3);
          if (s1) candSegs.push(s1);
          if (s2) candSegs.push(s2);
        }
        recordPillNeed(es.startVC, pillW);
      } else {
        const sx = vLaneX[es.startVC]['e_start:' + edge._idx];
        const hy = hLaneY[edge.waypoint_row]['e:' + edge._idx];
        const ex = vLaneX[es.endVC]['e_end:' + edge._idx];
        const startTap = pushSeg(pFrom.x, pFrom.y, sx, pFrom.y, {
          edgeIdx: edge._idx, fromID: edge.from, toID: edge.to, flowKind: edge.flow_kind,
          inferred: inferred, isTerminalFrom: true, corridorCol: es.startVC
        });
        const vStart = pushSeg(sx, pFrom.y, sx, hy, {
          edgeIdx: edge._idx, fromID: edge.from, toID: edge.to, flowKind: edge.flow_kind,
          inferred: inferred, corridorCol: es.startVC
        });
        const hSeg = pushSeg(sx, hy, ex, hy, {
          edgeIdx: edge._idx, fromID: edge.from, toID: edge.to, flowKind: edge.flow_kind,
          inferred: inferred, corridorRow: edge.waypoint_row
        });
        const vEnd = pushSeg(ex, hy, ex, pTo.y, {
          edgeIdx: edge._idx, fromID: edge.from, toID: edge.to, flowKind: edge.flow_kind,
          inferred: inferred, corridorCol: es.endVC
        });
        const tap = pushSeg(ex, pTo.y, pTo.x, pTo.y, {
          edgeIdx: edge._idx, fromID: edge.from, toID: edge.to, flowKind: edge.flow_kind,
          inferred: inferred, isTerminalTo: true, corridorCol: es.endVC
        });
        if (hSeg) candSegs.push(hSeg);
        if (tap) candSegs.push(tap);
        if (startTap) candSegs.push(startTap);
        if (vEnd) candSegs.push(vEnd);
        if (vStart) candSegs.push(vStart);
      }

      arrowHeads.push({ x: pTo.x, y: pTo.y, side: pTo.side, flowKind: edge.flow_kind });
      pillSpecs.push({ edge: edge, label: label, width: pillW, segments: candSegs, color: color });
    });

    // Step 5: Place <= 16-char verb pills in collision-free spans.
    const placedPills = [];
    function pillIsClear(rect) {
      for (let i = 0; i < geom.cardList.length; i++) {
        if (rectsOverlap(rect, geom.cardList[i], CARD_CLEARANCE)) return false;
      }
      for (let i = 0; i < geom.badges.length; i++) {
        if (rectsOverlap(rect, geom.badges[i], BADGE_CLEARANCE)) return false;
      }
      for (let i = 0; i < placedPills.length; i++) {
        if (rectsOverlap(rect, placedPills[i].rect, PILL_CLEARANCE)) return false;
      }
      return true;
    }

    pillSpecs.forEach(function (spec) {
      if (!spec.label || spec.segments.length === 0) return;
      const w = spec.width;
      const h = PILL_HEIGHT;
      const candidates = [];

      spec.segments.forEach(function (seg) {
        if (seg.isHorizontal) {
          const minX = Math.min(seg.x1, seg.x2);
          const maxX = Math.max(seg.x1, seg.x2);
          const span = maxX - minX;
          const midX = (minX + maxX) / 2;
          const xs = [midX];
          if (seg.corridorCol !== undefined && geom.vCorridors[seg.corridorCol]) {
            const vc = geom.vCorridors[seg.corridorCol];
            xs.push(vc.cx, vc.left + w / 2 + 6, vc.right - w / 2 - 6);
          }
          if (span > w + 12) {
            const usableMin = minX + w / 2 + 6;
            const usableMax = maxX - w / 2 - 6;
            const usableSpan = usableMax - usableMin;
            [0.3, 0.7, 0.15, 0.85, 0.4, 0.6, 0.0, 1.0].forEach(function (f) {
              xs.push(usableMin + usableSpan * f);
            });
            for (let x = usableMin; x <= usableMax; x += 24) {
              xs.push(x);
            }
          }
          const yOffsets = [0, -12, 12, -22, 22];
          yOffsets.forEach(function (dy) {
            xs.forEach(function (cx) {
              candidates.push({ cx: cx, cy: seg.y1 + dy });
            });
          });
        } else {
          const minY = Math.min(seg.y1, seg.y2);
          const maxY = Math.max(seg.y1, seg.y2);
          const xs = [seg.x1];
          if (seg.corridorCol !== undefined && geom.vCorridors[seg.corridorCol]) {
            xs.push(geom.vCorridors[seg.corridorCol].cx);
          }
          const fractions = [0.5, 0.3, 0.7, 0.18, 0.82];
          xs.forEach(function (cx) {
            fractions.forEach(function (f) {
              candidates.push({ cx: cx, cy: minY + (maxY - minY) * f });
            });
          });
        }
      });

      let chosen = null;
      for (let i = 0; i < candidates.length; i++) {
        const c = candidates[i];
        const rect = { left: c.cx - w / 2, top: c.cy - h / 2, right: c.cx + w / 2, bottom: c.cy + h / 2 };
        if (pillIsClear(rect)) {
          chosen = { cx: c.cx, cy: c.cy, rect: rect };
          break;
        }
      }
      if (!chosen) {
        const fallback = candidates[0];
        chosen = {
          cx: fallback.cx,
          cy: fallback.cy,
          rect: { left: fallback.cx - w / 2, top: fallback.cy - h / 2, right: fallback.cx + w / 2, bottom: fallback.cy + h / 2 }
        };
      }
      placedPills.push({
        edge: spec.edge,
        label: spec.label,
        color: spec.color,
        cx: chosen.cx,
        cy: chosen.cy,
        width: w,
        height: h,
        rect: chosen.rect
      });
    });

    // Step 6: Strict Collision & Crossing Verification.
    const collisions = [];
    const viewID = viewData.id;

    // 6a. Segment vs non-incident card (4px clearance) & incident interior penetration.
    for (let i = 0; i < segments.length; i++) {
      const seg = segments[i];
      for (let j = 0; j < geom.cardList.length; j++) {
        const card = geom.cardList[j];
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

      // 6b. Segment vs container badge (4px clearance).
      for (let j = 0; j < geom.badges.length; j++) {
        const badge = geom.badges[j];
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

    // 6c. Verb pill vs card, badge, and other verb pills (4px clearance).
    for (let i = 0; i < placedPills.length; i++) {
      const p = placedPills[i];
      const pName = p.edge.from + '->' + p.edge.to + ':' + p.label;
      for (let j = 0; j < geom.cardList.length; j++) {
        if (rectsOverlap(p.rect, geom.cardList[j], CARD_CLEARANCE)) {
          collisions.push({
            view_id: viewID,
            kind: 'pill_card_collision',
            subjects: [pName, geom.cardList[j].id],
            detail: 'verb pill overlaps card ' + geom.cardList[j].id
          });
        }
      }
      for (let j = 0; j < geom.badges.length; j++) {
        if (rectsOverlap(p.rect, geom.badges[j], BADGE_CLEARANCE)) {
          collisions.push({
            view_id: viewID,
            kind: 'pill_badge_collision',
            subjects: [pName, geom.badges[j].id],
            detail: 'verb pill overlaps container badge ' + geom.badges[j].id
          });
        }
      }
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

    // 6d. Parallel collinear segment overlaps (>= 6px separation) and perpendicular bridges.
    const bridgesBySegIdx = {};
    for (let i = 0; i < segments.length; i++) {
      const a = segments[i];
      for (let j = i + 1; j < segments.length; j++) {
        const b = segments[j];
        if (a.edgeIdx === b.edgeIdx) continue;

        if (a.isHorizontal && b.isHorizontal) {
          if (Math.abs(a.y1 - b.y1) < COLLINEAR_MIN_SEP - 0.1) {
            const ovStart = Math.max(Math.min(a.x1, a.x2), Math.min(b.x1, b.x2));
            const ovEnd = Math.min(Math.max(a.x1, a.x2), Math.max(b.x1, b.x2));
            if (ovEnd - ovStart > 1) {
              collisions.push({
                view_id: viewID,
                kind: 'collinear_collision',
                subjects: [a.fromID + '->' + a.toID, b.fromID + '->' + b.toID],
                detail: 'parallel horizontal segments closer than 6px'
              });
            }
          }
        } else if (a.isVertical && b.isVertical) {
          if (Math.abs(a.x1 - b.x1) < COLLINEAR_MIN_SEP - 0.1) {
            const ovStart = Math.max(Math.min(a.y1, a.y2), Math.min(b.y1, b.y2));
            const ovEnd = Math.min(Math.max(a.y1, a.y2), Math.max(b.y1, b.y2));
            if (ovEnd - ovStart > 1) {
              collisions.push({
                view_id: viewID,
                kind: 'collinear_collision',
                subjects: [a.fromID + '->' + a.toID, b.fromID + '->' + b.toID],
                detail: 'parallel vertical segments closer than 6px'
              });
            }
          }
        } else {
          if (a.busGroup && b.busGroup && a.busGroup === b.busGroup) continue;
          const hSeg = a.isHorizontal ? a : b;
          const hIdx = a.isHorizontal ? i : j;
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
      }
    }

    return {
      segments: segments,
      junctions: junctions,
      arrowHeads: arrowHeads,
      placedPills: placedPills,
      bridgesBySegIdx: bridgesBySegIdx,
      collisions: collisions,
      vLaneCounts: vLaneCounts,
      vPillReserve: vPillReserve,
      hLaneCounts: hLaneCounts,
      hBadgeReserve: hBadgeReserve
    };
  }

  function applyCorridorWidening(sheet, geom, passRes) {
    let widened = false;
    Object.keys(geom.vCorridors).forEach(function (cKey) {
      const cIdx = parseInt(cKey, 10);
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
      const rIdx = parseInt(rKey, 10);
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
        const cIdx = parseInt(cKey, 10);
        const vc = geom.vCorridors[cIdx];
        if ((passRes.vLaneCounts[cIdx] || 0) > 0 && vc.width < MAX_CORRIDOR_SIZE - 1) {
          sheet.style.setProperty('--corridor-col-' + cIdx, MAX_CORRIDOR_SIZE + 'px');
          widened = true;
        }
      });
      Object.keys(geom.hCorridors).forEach(function (rKey) {
        const rIdx = parseInt(rKey, 10);
        const hc = geom.hCorridors[rIdx];
        if ((passRes.hLaneCounts[rIdx] || 0) > 0 && hc.height < MAX_CORRIDOR_SIZE - 1) {
          sheet.style.setProperty('--corridor-row-' + rIdx, MAX_CORRIDOR_SIZE + 'px');
          widened = true;
        }
      });
    }

    if (widened) {
      void sheet.offsetHeight;
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
      parts.push('L', startX.toFixed(1), y.toFixed(1));
      parts.push('A', BRIDGE_RADIUS, BRIDGE_RADIUS, 0, 0, sweep, endX.toFixed(1), y.toFixed(1));
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
      if (seg.isHorizontal && bridges && bridges.length > 0) {
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

    if (document.fonts && document.fonts.ready) {
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
        if (!widened && passRes.collisions.length === 0) {
          break;
        }
        if (!widened) {
          break;
        }
      }

      if (passRes.collisions.length > 0) {
        for (let k = 0; k < passRes.collisions.length; k++) {
          allCollisions.push(passRes.collisions[k]);
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
