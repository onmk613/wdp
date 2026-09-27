// ESLint flat config：Vue 3 + TypeScript（vue3-recommended + ts-eslint
// recommended 为基线）。定位是「能拦真问题」——纯排版/风格类规则一律
// 关闭（本仓格式由既有代码风格约定，不做全量重排）；未使用变量等可
// 疑项给 warn 不阻断。typecheck 已由 vue-tsc 负责，这里不重复做类型
// 层检查（projectService 不开，保持 lint 秒级）。
import { defineConfigWithVueTs, vueTsConfigs } from '@vue/eslint-config-typescript'
import pluginVue from 'eslint-plugin-vue'

export default defineConfigWithVueTs(
  {
    name: 'wdp/files-to-lint',
    files: ['src/**/*.{ts,vue}'],
  },
  {
    name: 'wdp/ignores',
    ignores: ['dist/**', 'node_modules/**'],
  },
  pluginVue.configs['flat/recommended'],
  vueTsConfigs.recommended,
  {
    name: 'wdp/rules',
    rules: {
      // ---- 拦真问题（error）----
      'no-unused-vars': 'off', // 由 TS 版接管（含类型位置）
      '@typescript-eslint/no-unused-vars': ['error', {
        argsIgnorePattern: '^_', // 惯例：下划线前缀 = 有意忽略
        caughtErrors: 'none', // catch (e) 不用 e 很常见（按 message 提示）
      }],
      'no-undef': 'off', // TS 编译器已覆盖（且会对 .vue 里的类型误报）
      'vue/no-unused-vars': 'error', // 模板里 v-for/v-slot 未用变量
      'vue/no-use-v-if-with-v-for': 'error',
      'vue/no-side-effects-in-computed-properties': 'error',
      'vue/require-v-for-key': 'error',
      'vue/no-duplicate-attributes': 'error',
      'vue/no-mutating-props': 'error',
      'vue/no-template-key': 'error',
      'vue/no-useless-template-attributes': 'error',
      'vue/return-in-computed-property': 'error',
      'vue/no-async-in-computed-properties': 'error',

      // ---- 可疑但常有正当理由（warn，不阻断）----
      '@typescript-eslint/no-explicit-any': 'warn', // 不强制 error：存量代码有刻意 any（yaml AST）
      '@typescript-eslint/no-non-null-assertion': 'warn',
      '@typescript-eslint/ban-ts-comment': 'warn',
      'vue/block-order': 'warn', // script-setup 在前、template 在后
      'vue/no-v-html': 'warn',

      // ---- 纯排版/风格：关闭（不做全量重排，保持与既有代码一致）----
      'vue/max-attributes-per-line': 'off',
      'vue/singleline-html-element-content-newline': 'off',
      'vue/multiline-html-element-content-newline': 'off',
      'vue/html-indent': 'off',
      'vue/html-self-closing': 'off',
      'vue/html-closing-bracket-newline': 'off',
      'vue/html-closing-bracket-spacing': 'off',
      'vue/first-attribute-linebreak': 'off',
      'vue/attributes-order': 'off',
      'vue/order-in-components': 'off', // script-setup 组件无此形态
      'vue/component-definition-name-casing': 'off',
      'vue/one-component-per-file': 'off',
      'vue/prefer-true-attribute-shorthand': 'off',
      'vue/require-default-prop': 'off',
      'vue/require-prop-types': 'off',
      'vue/multi-word-component-names': 'off', // 视图按路由单文件命名是既定约定
      'vue/html-quotes': 'off',
      'vue/comment-directive': 'off',
      'vue/no-reserved-component-names': 'off',
      '@typescript-eslint/no-empty-object-type': 'off',
      '@typescript-eslint/no-wrapper-object-types': 'off',
      '@typescript-eslint/consistent-type-imports': 'off',
    },
  },
)
