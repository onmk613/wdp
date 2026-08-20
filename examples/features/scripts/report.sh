#!/bin/sh
# script 模块演示：控制端脚本上传到目标机临时路径执行，结束自删。
# 参数（free-form）原样透传，目标机需有 shebang 对应的解释器。
set -eu
echo "report from $(hostname) at $(date '+%Y-%m-%d %H:%M:%S')"
echo "args: $*"
