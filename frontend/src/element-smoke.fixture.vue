<!-- 按需引入冒烟夹具（element-smoke.spec.ts 消费）：模板里只有 el-* 标签，
     不做任何手动注册——组件能否渲染全靠 vite.config.ts 的 Components 插件
     在编译期就地 import。locale 经根组件同款 ElConfigProvider 提供。 -->
<script setup lang="ts">
import zhCn from 'element-plus/dist/locale/zh-cn.mjs'
</script>

<template>
  <el-config-provider :locale="zhCn">
    <el-button data-testid="btn">按钮</el-button>
    <el-pagination data-testid="pager" :total="42" />
  </el-config-provider>
</template>
