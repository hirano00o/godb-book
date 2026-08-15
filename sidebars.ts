import type { SidebarsConfig } from "@docusaurus/plugin-content-docs";

// 章を追加したら items に 1 行ずつ追記する(部ごとに category を作る)
const sidebars: SidebarsConfig = {
    book: [
        "index",
        {
            type: "category",
            label: "第1部 データベースの基礎とストレージ層",
            collapsed: false,
            items: ["part1/ch01", "part1/ch02", "part1/ch03", "part1/ch04", "part1/afterword"],
        },
        {
            type: "category",
            label: "第2部 B-Tree",
            collapsed: false,
            items: ["part2/ch05", "part2/ch06", "part2/ch07", "part2/ch08", "part2/afterword"],
        },
        {
            type: "category",
            label: "第3部 SQL 実行エンジン",
            collapsed: false,
            items: ["part3/ch09", "part3/ch10", "part3/ch11", "part3/ch12", "part3/afterword"],
        },
    ],
};

export default sidebars;
