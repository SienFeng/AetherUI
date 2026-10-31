// 用量图共用的 Chart.js 配置。
//
// 两张图——入站展开行里的单用户图、系统状态页的全用户图——只在 x 轴刻度
// 上限上有差别，其余完全一致。不重复两份：两份配置迟早会分叉，而分叉后
// 两张图的观感差异不会有任何东西提醒你。
//
// 依赖全局的 sizeFormat（common.js）与 Chart（chart.umd.min.js），因此只在
// 引入了这两者的页面里可用。
function trafficChartOptions(maxTicksLimit) {
    return {
        responsive: true,
        maintainAspectRatio: false,
        interaction: { mode: 'index', intersect: false },
        plugins: {
            legend: { position: 'top' },
            tooltip: {
                // 不用 Chart.js 自带的悬浮框：它画在 canvas 里，超出画布的部分直接被裁掉。
                // 系统状态页的一屏布局下图只有一百多 px 高，八九个入站的列表下半截会看不见。
                // 改由 externalChartTooltip 渲染成挂在 body 上的 HTML 框，不受图表与卡片边界限制。
                enabled: false,
                external: externalChartTooltip,
                // 按悬停这一刻的用量降序，而不是 Chart.js 默认的数据集顺序——后者
                // 是服务端按「整个时间范围的总量」排出来的（Overview 的
                // `total desc`），与当前时间点无关：总量第一的入站在某个小时
                // 可能是 0 B，却仍排在最上面。图例不跟着排，它决定颜色分配，
                // 跨时间点必须稳定。
                itemSort: (a, b) => b.parsed.y - a.parsed.y,
                callbacks: {
                    label: ctx => ctx.dataset.label + ': ' + sizeFormat(ctx.parsed.y),
                },
            },
        },
        scales: {
            y: {
                beginAtZero: true,
                ticks: { callback: value => sizeFormat(value) },
            },
            x: {
                ticks: { maxRotation: 45, minRotation: 0, autoSkip: true, maxTicksLimit: maxTicksLimit },
            },
        },
    };
}

// 用量轴的整刻度。Chart.js 按十进制取整（2e9、4e9…），而 sizeFormat 按
// 1024 换算，两者一叠加就是 1.86 GB / 3.73 GB 这种刻度。这里先选一个
// 1024 的幂作单位，再在该单位下取 1 / 2 / 2.5 / 5 × 10^n 的步长，约 4 格，
// 得到 0 / 4 / 8 / 12 / 16 GB 这样的刻度。
//
// 全为 0 时（新装、或这段时间确实没人用）固定给 0 ~ 1 GB：不给的话 Chart.js
// 会画出 0 ~ 1 字节、刻度是「1 B / 0 B」的轴。
function niceByteAxis(maxValue) {
    if (!(maxValue > 0)) {
        return { stepSize: 256 * 1024 * 1024, max: 1024 * 1024 * 1024 };
    }
    let unit = 1;
    while (maxValue / unit >= 1024) {
        unit *= 1024;
    }
    const raw = maxValue / unit / 4;
    const mag = Math.pow(10, Math.floor(Math.log10(raw)));
    const step = [1, 2, 2.5, 5, 10].map(m => m * mag).find(v => v >= raw) * unit;
    return { stepSize: step, max: Math.ceil(maxValue / step) * step };
}

// 轴刻度用的短格式：去掉末尾多余的 0（1.50 GB → 1.5 GB，512.00 MB → 512 MB），0 只写 0。
function shortSize(v) {
    if (v === 0) return '0';
    const parts = sizeFormat(v).split(' ');
    return parseFloat(parts[0]) + ' ' + parts[1];
}

// 两张用量图共用的外观：整刻度 y 轴、浅色横向网格、不画竖网格、灰色刻度字。
// maxValue 是图上最高点（堆叠图传各点之和的最大值）。在 trafficChartOptions
// 的返回值上就地修改，返回同一个对象。
function styleTrafficAxes(options, maxValue) {
    const axis = niceByteAxis(maxValue);
    const tick = '#64748B';
    options.scales.y.max = axis.max;
    options.scales.y.ticks.stepSize = axis.stepSize;
    options.scales.y.ticks.callback = shortSize;
    options.scales.y.ticks.color = tick;
    options.scales.y.grid = { color: '#EEF1F6' };
    options.scales.y.border = { display: false };
    options.scales.x.grid = { display: false };
    options.scales.x.border = { color: '#E5E9F0' };
    options.scales.x.ticks.color = tick;
    options.scales.x.ticks.maxRotation = 0;
    return options;
}

// Chart.js 的 external 悬浮框：内容取自 Chart.js 已经算好的 tooltip 模型（标题、
// 每行文本、颜色、footer），所以各页对 filter / itemSort / callbacks 的覆盖照常生效。
// 整页只用一个元素，position: fixed 挂在 body 上；外观在 custom.css 的 .chart-tooltip。
// 文本一律走 textContent——入站备注是管理员可写的任意字符串，不能拼进 innerHTML。
function externalChartTooltip(context) {
    const tooltip = context.tooltip;
    let el = document.getElementById('chart-tooltip');
    if (!el) {
        el = document.createElement('div');
        el.id = 'chart-tooltip';
        el.className = 'chart-tooltip';
        el.setAttribute('aria-hidden', 'true');
        document.body.appendChild(el);
        // 滚轮滚动时鼠标仍停在画布上，Chart.js 不会收到离开事件；固定定位的框会和图错位，直接收起。
        // 只认包含这张图的滚动容器：页面里还有排行表、节点列表等各自滚动的区域，
        // 它们的滚动与图的位置无关，一律收起会让框刚出现就消失。
        window.addEventListener('scroll', e => {
            const canvas = el._canvas;
            const t = e.target;
            if (canvas && (t === document || (t.contains && t.contains(canvas)))) {
                el.style.opacity = '0';
            }
        }, true);
    }
    el._canvas = context.chart.canvas;
    if (tooltip.opacity === 0) {
        el.style.opacity = '0';
        return;
    }

    el.textContent = '';
    (tooltip.title || []).forEach(t => {
        const row = document.createElement('div');
        row.className = 'chart-tooltip-title';
        row.textContent = t;
        el.appendChild(row);
    });
    (tooltip.body || []).forEach((b, i) => {
        const row = document.createElement('div');
        row.className = 'chart-tooltip-row';
        const swatch = document.createElement('i');
        const c = tooltip.labelColors[i] || {};
        swatch.style.background = c.backgroundColor || c.borderColor || '#94A3B8';
        row.appendChild(swatch);
        const text = document.createElement('span');
        text.textContent = b.lines.join(' ');
        row.appendChild(text);
        el.appendChild(row);
    });
    (tooltip.footer || []).forEach(t => {
        const row = document.createElement('div');
        row.className = 'chart-tooltip-footer';
        row.textContent = t;
        el.appendChild(row);
    });

    // 默认放在悬停点右侧、与悬停点垂直居中；右边放不下翻到左侧，上下夹在视口内
    const rect = context.chart.canvas.getBoundingClientRect();
    const x = rect.left + tooltip.caretX;
    const y = rect.top + tooltip.caretY;
    el.style.opacity = '1';
    const w = el.offsetWidth;
    const h = el.offsetHeight;
    const gap = 12;
    const margin = 8;
    let left = x + gap;
    if (left + w > window.innerWidth - margin) {
        left = Math.max(margin, x - gap - w);
    }
    const top = Math.min(Math.max(margin, y - h / 2), window.innerHeight - h - margin);
    el.style.left = left + 'px';
    el.style.top = top + 'px';
}
