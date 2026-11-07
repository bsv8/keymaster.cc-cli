// Package collector 实现 CLI 侧的采集上报：Catalog 建映射后入队、
// 目录核对发现名称变化、单独持久化、钱包签名、失败重试。
// 提交状态与 Catalog 内容映射分开存放；目录路径不上报。
package collector
