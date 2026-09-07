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
