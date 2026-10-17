// d3 charts as Datastar Rocket web components.
//
// The server stays in charge: it renders each chart as a bare custom element
// whose attributes carry the data, e.g.
//
//   <chart-timeseries id="chart-01J…" points='[[1759302000000,175],…]' average='[…]' unit="lb">
//
// and later morphs change those attributes. Rocket decodes each attribute
// through its prop codec and calls observeProps, and d3 redraws with a
// transition. Components render into an open shadow root, so the page morph
// never touches what d3 drew. Nothing here holds application state.

import { rocket } from 'datastar'
import * as d3 from 'd3'

const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)')
const duration = () => (reduceMotion.matches ? 0 : 300)
const fmt = d3.format(',.4~r')
const day = d3.utcFormat('%b %-d')

// Colors come from the page's custom properties, which inherit through the
// shadow boundary, so light and dark follow the page.
const STYLE = `
  :host { display: block; position: relative; }
  svg { display: block; width: 100%; overflow: visible; }
  text { font: 11px system-ui, sans-serif; fill: var(--ink-muted); }
  .grid line { stroke: var(--grid); }
  .reading { fill: var(--cat-1); stroke: var(--surface); stroke-width: 2; }
  .avg { fill: none; stroke: var(--cat-2); stroke-width: 2; stroke-linejoin: round; stroke-linecap: round; }
  .end { font-size: 12px; fill: var(--ink); }
  .legend text { font-size: 12px; fill: var(--ink); }
  .cross { stroke: var(--ink-muted); stroke-dasharray: 2 2; }
  .hit { fill: transparent; }
  .tip { position: absolute; pointer-events: none; white-space: nowrap; z-index: 1;
         font: 13px/1.3 system-ui, sans-serif; padding: .3rem .5rem; border-radius: 6px;
         color: var(--ink); background: var(--surface);
         border: 1px solid color-mix(in srgb, var(--ink) 20%, transparent);
         box-shadow: 0 2px 8px rgb(0 0 0 / .15); transform: translate(-50%, calc(-100% - 10px)); }
  .tip[hidden] { display: none; }
`

// Readings as dots and their trailing 7-day average as a line, on a time
// axis. Two series, so a legend plus a direct label on the line's end.
rocket('chart-timeseries', {
  mode: 'open',
  props: ({ json, string, number }) => ({
    points: json.default(() => []),
    average: json.default(() => []),
    unit: string.default(''),
    label: string.default('time series'),
    height: number.default(220),
  }),
  // d3 owns the DOM after the first render; prop changes redraw, not re-render.
  renderOnPropChange: false,
  render: ({ html }) => html`<style>${STYLE}</style><svg></svg><div class="tip" hidden></div>`,
  onFirstRender: ({ host, props, observeProps, cleanup }) => {
    const root = host.shadowRoot
    const svg = d3.select(root.querySelector('svg'))
    const tip = root.querySelector('.tip')
    let width = 0
    const redraw = (animate) => {
      width = host.clientWidth || width
      if (width) draw(svg, tip, props, width, animate ? duration() : 0)
    }
    observeProps(() => redraw(true))
    const ro = new ResizeObserver(() => {
      if (host.clientWidth !== width) redraw(false)
    })
    ro.observe(host)
    cleanup(() => ro.disconnect())
    redraw(false)
  },
})

function draw(svg, tip, props, width, t) {
  const pts = props.points.map(([x, y]) => ({ x: new Date(x), y: +y }))
  const avg = props.average.map(([x, y]) => ({ x: new Date(x), y: +y }))
  const unit = props.unit ? ` ${props.unit}` : ''
  const legendH = 22
  const h = props.height + legendH
  const m = { top: 10 + legendH, right: 72, bottom: 24, left: 44 }
  svg.attr('viewBox', `0 0 ${width} ${h}`).attr('height', h).attr('role', 'img').attr('aria-label', props.label)

  const xExt = d3.extent(pts, (d) => d.x)
  if (xExt[0] && +xExt[0] === +xExt[1]) xExt[0] = d3.utcDay.offset(xExt[0], -1)
  const x = d3.scaleUtc().domain(xExt[0] ? xExt : [d3.utcDay.offset(new Date(), -7), new Date()]).range([m.left, width - m.right])
  // Readings like body weight sit far from zero; a zero baseline would
  // flatten every change, so the y domain hugs the data.
  const ys = pts.map((d) => d.y).concat(avg.map((d) => d.y))
  const [lo, hi] = d3.extent(ys.length ? ys : [0, 1])
  const pad = Math.max((hi - lo) * 0.15, Math.abs(hi) * 0.01, 0.5)
  const y = d3.scaleLinear().domain([lo - pad, hi + pad]).nice(4).range([h - m.bottom, m.top])

  svg.selectAll('g.grid').data([0]).join('g').attr('class', 'grid')
    .attr('transform', `translate(${m.left},0)`)
    .transition().duration(t)
    .call(d3.axisLeft(y).ticks(4).tickSize(-(width - m.left - m.right)).tickFormat(fmt))
    .call((g) => g.select('.domain').remove())
    .call((g) => g.selectAll('text').attr('x', -6))
  svg.selectAll('g.x').data([0]).join('g').attr('class', 'x')
    .attr('transform', `translate(0,${h - m.bottom})`)
    .transition().duration(t)
    .call(d3.axisBottom(x).ticks(Math.max(2, Math.floor((width - m.left - m.right) / 80))).tickFormat(day).tickSize(0).tickPadding(8))
    .call((g) => g.select('.domain').remove())

  const line = d3.line().x((d) => x(d.x)).y((d) => y(d.y))
  svg.selectAll('path.avg').data([avg]).join('path').attr('class', 'avg')
    .transition().duration(t).attr('d', line)
  svg.selectAll('circle.reading').data(pts, (d) => +d.x)
    .join((enter) => enter.append('circle').attr('class', 'reading').attr('r', 4)
      .attr('cx', (d) => x(d.x)).attr('cy', (d) => y(d.y)))
    .transition().duration(t).attr('cx', (d) => x(d.x)).attr('cy', (d) => y(d.y))

  // Direct label on the average line's last point.
  const last = avg[avg.length - 1]
  svg.selectAll('text.end').data(last ? [last] : []).join('text').attr('class', 'end')
    .attr('x', (d) => x(d.x) + 8).attr('y', (d) => y(d.y) + 4).text('7-day avg')

  // Legend: one row above the plot.
  const lg = svg.selectAll('g.legend').data([0]).join('g').attr('class', 'legend')
    .attr('transform', `translate(${m.left},4)`)
  lg.selectAll('circle').data([0]).join('circle').attr('class', 'reading').attr('r', 4).attr('cx', 4).attr('cy', 8)
  lg.selectAll('text.k1').data([0]).join('text').attr('class', 'k1').attr('x', 14).attr('y', 12).text('reading')
  lg.selectAll('rect').data([0]).join('rect').attr('x', 78).attr('y', 6.5).attr('width', 14).attr('height', 3).attr('rx', 1.5)
    .attr('fill', 'var(--cat-2)')
  lg.selectAll('text.k2').data([0]).join('text').attr('class', 'k2').attr('x', 96).attr('y', 12).text('7-day average')

  // Crosshair and tooltip at the nearest reading.
  const cross = svg.selectAll('line.cross').data([0]).join('line').attr('class', 'cross')
    .attr('y1', m.top).attr('y2', h - m.bottom).attr('visibility', 'hidden')
  const bisect = d3.bisector((d) => d.x).center
  svg.selectAll('rect.hit').data([0]).join('rect').attr('class', 'hit')
    .attr('x', m.left - 8).attr('y', m.top).attr('width', width - m.left - m.right + 16).attr('height', h - m.top - m.bottom)
    .on('pointermove', (evt) => {
      if (!pts.length) return
      const i = bisect(pts, x.invert(d3.pointer(evt, svg.node())[0]))
      const p = pts[i], a = avg[i]
      cross.attr('x1', x(p.x)).attr('x2', x(p.x)).attr('visibility', 'visible')
      tip.textContent = `${day(p.x)}: ${fmt(p.y)}${unit}` + (a ? ` · avg ${fmt(a.y)}${unit}` : '')
      tip.style.left = `${x(p.x)}px`
      tip.style.top = `${Math.min(y(p.y), a ? y(a.y) : Infinity)}px`
      tip.hidden = false
    })
    .on('pointerleave', () => { cross.attr('visibility', 'hidden'); tip.hidden = true })
}
