import type { SidebarsConfig } from "@docusaurus/plugin-content-docs";

// 章を追加したら items に 1 行ずつ追記する(部ごとに category を作る)
const sidebars: SidebarsConfig = {
    book: [
        "index",
        {
            type: "category",
            label: "第1部 データベースの基礎とストレージ層",
            collapsed: false,
            items: ["part1/ch01", "part1/ch02", "part1/ch03"],
        },
    ],
};

export default sidebars;
