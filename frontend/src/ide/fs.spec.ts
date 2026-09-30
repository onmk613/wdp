// ChartFS 与 chart.yaml 工具函数单测：脏判定、序列化（保存请求体）、
// 重命名/删除语义（曾出现「重命名保存后新旧并存/静默丢失」的数据问题）。
import { describe, expect, it } from 'vitest'
import {
  ChartFS, bumpVersion, chartYAMLDescription, chartYAMLVersion, isReserved,
  nextVersion, patchChartYAMLDescription, patchChartYAMLVersion, pathOk,
} from './fs'
import type { AppSpec } from '../api'

function specFixture(): AppSpec {
  return {
    name: 'demo',
    version: '1.0.0',
    description: '',
    values_yaml: 'app:\n  port: 80\n',
    deploy_yaml: '- name: p1\n  hosts: all\n  tasks:\n    - shell: echo hi\n',
    files: [
      { path: 'chart.yaml', content: '# meta\nname: demo\nversion: 1.0.0\n', size: 30 },
      { path: 'files/old.conf', content: 'OLD', size: 3 },
      { path: 'bin/logo.png', binary: true, size: 1024 },
    ],
    pools: [],
    groups: [],
    labels: '{}',
  }
}

function newFS(): ChartFS {
  const fs = new ChartFS()
  fs.loadFromSpec(specFixture(), '1.0.0')
  return fs
}

// ---- pathOk ----
describe('pathOk', () => {
  it('接受相对路径', () => {
    expect(pathOk('files/a.conf')).toBe(true)
    expect(pathOk('tasks/main.yaml')).toBe(true)
    expect(pathOk('a.yaml')).toBe(true)
  })
  it('拒绝越界/非法形态', () => {
    expect(pathOk('')).toBe(false)
    expect(pathOk('../etc/passwd')).toBe(false)
    expect(pathOk('/abs/path')).toBe(false)
    expect(pathOk('a\\b')).toBe(false)
    expect(pathOk('a//b')).toBe(false)
    expect(pathOk('a/./b')).toBe(false)
    expect(pathOk('a/b:c')).toBe(false)
    expect(pathOk('a/b')).toBe(true) // 对照
  })
})

// ---- chart.yaml 工具 ----
describe('chartYAML 工具', () => {
  it('chartYAMLVersion 解析（含引号形式）', () => {
    expect(chartYAMLVersion('name: a\nversion: 1.2.3\n')).toBe('1.2.3')
    expect(chartYAMLVersion('version: "2.0"\n')).toBe('2.0')
    expect(chartYAMLVersion('name: a\n')).toBe('')
  })
  it('patchChartYAMLVersion 替换首个顶层 version 行，保留注释', () => {
    const src = '# top\nname: demo # 行尾\nversion: 1.0.0\nrequired: [x]\n'
    const out = patchChartYAMLVersion(src, '2.0.0')
    expect(out).toBe('# top\nname: demo # 行尾\nversion: 2.0.0\nrequired: [x]\n')
  })
  it('patchChartYAMLVersion 无 version 行时插在 name 后', () => {
    const out = patchChartYAMLVersion('name: demo\n', '1.1.0')
    expect(out).toBe('name: demo\nversion: 1.1.0\n')
  })
  it('chartYAMLDescription 解析', () => {
    expect(chartYAMLDescription('description: hello\n')).toBe('hello')
    expect(chartYAMLDescription('name: a\n')).toBe('')
  })
  it('patchChartYAMLDescription 替换已有行', () => {
    expect(patchChartYAMLDescription('name: a\ndescription: old\n', 'new'))
      .toBe('name: a\ndescription: new\n')
  })
  it('patchChartYAMLDescription 无行时追加', () => {
    expect(patchChartYAMLDescription('name: a\n', 'd'))
      .toBe('name: a\ndescription: d\n')
  })
  it('patchChartYAMLDescription 特殊字符自动加引号', () => {
    const out = patchChartYAMLDescription('name: a\n', '含: 冒号')
    expect(out).toContain('"含: 冒号"')
  })
})

// ---- 版本推进 ----
describe('nextVersion/bumpVersion', () => {
  it('bump 末位数字', () => {
    expect(bumpVersion('1.0.9')).toBe('1.0.10')
    expect(bumpVersion('v2')).toBe('v3')
    expect(bumpVersion('')).toBe('1.0.0')
    expect(bumpVersion('release')).toBe('release.1')
  })
  it('nextVersion 跳过已占用号', () => {
    expect(nextVersion('1.0.0', ['1.0.1'])).toBe('1.0.2')
    expect(nextVersion('', [])).toBe('1.0.0')
  })
})

// ---- 保留文件守卫 ----
describe('RESERVED 守卫', () => {
  it('三件套不可删/不可重命名', () => {
    const fs = newFS()
    expect(fs.remove('chart.yaml')).toContain('不能删除')
    expect(fs.rename('values.yaml', 'v2.yaml')).toContain('不能重命名')
    expect(isReserved('deploy.yaml')).toBe(true)
  })
})

// ---- 脏判定与序列化 ----
describe('ChartFS 脏判定与 toSaveBody', () => {
  it('加载后干净；未改文件不回传（部分保存语义）', () => {
    const fs = newFS()
    expect(fs.isDirty()).toBe(false)
    const body = fs.toSaveBody('1.0.1')
    expect(body.delete_files).toEqual([])
    // 未改动：只有 chart.yaml（版本号对账需要恒回传）
    expect(body.files.map((f) => f.path)).toEqual(['chart.yaml'])
  })
  it('改动文件回传；清空文件以空串回传（不丢操作）', () => {
    const fs = newFS()
    fs.get('files/old.conf')!.content = ''
    const body = fs.toSaveBody('1.0.1')
    const entry = body.files.find((f) => f.path === 'files/old.conf')
    expect(entry).toBeDefined()
    expect(entry!.content).toBe('')
  })
  it('新建空文件以空串回传（后端按显式提交落盘）', () => {
    const fs = newFS()
    expect(fs.create('files/empty.conf', '')).toBeNull()
    const body = fs.toSaveBody('1.0.1')
    const entry = body.files.find((f) => f.path === 'files/empty.conf')
    expect(entry).toBeDefined()
    expect(entry!.content).toBe('')
  })
  it('删除进 delete_files，撤销删除还原', () => {
    const fs = newFS()
    expect(fs.remove('files/old.conf')).toBeNull()
    expect(fs.isDirty()).toBe(true)
    expect(fs.toSaveBody('1.0.1').delete_files).toEqual(['files/old.conf'])
    expect(fs.restore('files/old.conf')).toBeNull()
    expect(fs.isDirty()).toBe(false)
    expect(fs.toSaveBody('1.0.1').delete_files).toEqual([])
  })
  it('markSaved 重置基线', () => {
    const fs = newFS()
    fs.get('deploy.yaml')!.content = '- shell: v2\n'
    fs.remove('files/old.conf')
    fs.markSaved('1.0.1')
    expect(fs.isDirty()).toBe(false)
    expect(fs.baseVersion).toBe('1.0.1')
    expect(fs.get('files/old.conf')).toBeUndefined()
  })
})

// ---- 重命名语义（回归：曾静默丢失/新旧并存）----
describe('ChartFS.rename', () => {
  it('重命名底本文件 = 移动：旧路径进 delete_files，新路径内容回传', () => {
    const fs = newFS()
    expect(fs.rename('files/old.conf', 'files/new.conf')).toBeNull()
    // 仅重命名也算脏（此前返回 false → 「没有改动」→ 无法保存）
    expect(fs.isDirty()).toBe(true)
    const body = fs.toSaveBody('1.0.1')
    expect(body.delete_files).toContain('files/old.conf')
    const entry = body.files.find((f) => f.path === 'files/new.conf')
    expect(entry).toBeDefined()
    expect(entry!.content).toBe('OLD')
    // 旧路径不再作为普通文件回传
    expect(body.files.some((f) => f.path === 'files/old.conf')).toBe(false)
  })
  it('重命名+编辑：新内容随新路径回传', () => {
    const fs = newFS()
    fs.rename('files/old.conf', 'files/new.conf')
    fs.get('files/new.conf')!.content = 'NEW'
    const body = fs.toSaveBody('1.0.1')
    expect(body.files.find((f) => f.path === 'files/new.conf')!.content).toBe('NEW')
    expect(body.delete_files).toContain('files/old.conf')
  })
  it('重命名本会话新建的文件：不留占位（旧路径不在包里）', () => {
    const fs = newFS()
    fs.create('files/tmp.txt', 'T')
    fs.rename('files/tmp.txt', 'files/tmp2.txt')
    const body = fs.toSaveBody('1.0.1')
    expect(body.delete_files).toEqual([])
    expect(body.files.find((f) => f.path === 'files/tmp2.txt')!.content).toBe('T')
  })
  it('撤销重命名（rename 回去）：旧路径占位清除，文件回到原位', () => {
    const fs = newFS()
    fs.rename('files/old.conf', 'files/new.conf')
    expect(fs.rename('files/new.conf', 'files/old.conf')).toBeNull()
    const body = fs.toSaveBody('1.0.1')
    // 回到原位：old.conf 内容回传（仍视为改动，幂等无害），new.conf 不在包里
    expect(body.files.find((f) => f.path === 'files/old.conf')!.content).toBe('OLD')
    expect(body.files.some((f) => f.path === 'files/new.conf')).toBe(false)
    // spurious delete（new.conf 从未入库，后端容忍不存在）不影响正确性
  })
  it('目标路径被现存文件占用时报错；被删除占位占用时允许覆盖', () => {
    const fs = newFS()
    fs.create('files/exists.conf', 'x')
    expect(fs.rename('files/old.conf', 'files/exists.conf')).toContain('已存在')
    fs.remove('files/exists.conf')
    expect(fs.rename('files/old.conf', 'files/exists.conf')).toBeNull()
  })
  it('删除占位可恢复（=找回底本文件）', () => {
    const fs = newFS()
    fs.rename('files/old.conf', 'files/new.conf')
    expect(fs.restore('files/old.conf')).toBeNull()
    // 恢复后两处都在（用户显式撤销了移动），old 回到基线、new 待回传
    const body = fs.toSaveBody('1.0.1')
    expect(body.files.find((f) => f.path === 'files/new.conf')).toBeDefined()
    expect(body.delete_files).toEqual([])
  })
})

// ---- 新建模式脚手架基线（回归：未编辑也弹「未保存修改」确认）----
describe('ChartFS 脚手架基线', () => {
  it('加载脚手架后未编辑不算脏（离开不弹确认）', () => {
    const fs = new ChartFS()
    fs.loadFromScaffold('demo', '1.0.0', '')
    expect(fs.isDirty()).toBe(false)
    expect(fs.dirtyCount()).toBe(0)
  })
  it('编辑脚手架文件才算脏；改回原文恢复干净', () => {
    const fs = new ChartFS()
    fs.loadFromScaffold('demo', '1.0.0', '')
    const before = fs.get('deploy.yaml')!.content
    fs.get('deploy.yaml')!.content = '- shell: echo hi\n'
    expect(fs.isDirty()).toBe(true)
    fs.get('deploy.yaml')!.content = before
    expect(fs.isDirty()).toBe(false)
  })
  it('未编辑也全量回传（新建必须落全部文件）', () => {
    const fs = new ChartFS()
    fs.loadFromScaffold('demo', '1.0.0', '')
    const body = fs.toSaveBody('1.0.0')
    expect(body.files.map((f) => f.path).sort())
      .toEqual(['chart.yaml', 'deploy.yaml', 'values.yaml'])
  })
})

// ---- 草稿快照往返 ----
describe('ChartFS 草稿往返', () => {
  it('重命名后走草稿恢复再保存：仍是移动而非复制', () => {
    const fs = newFS()
    fs.rename('files/old.conf', 'files/new.conf')
    const payload = fs.toDraftPayload(['deploy.yaml'], 'deploy.yaml')
    const fs2 = new ChartFS()
    fs2.loadFromDraft(payload)
    const body = fs2.toSaveBody('1.0.1')
    expect(body.delete_files).toContain('files/old.conf')
    expect(body.files.find((f) => f.path === 'files/new.conf')!.content).toBe('OLD')
  })
  it('二进制文件随快照保留 binary 标记与大小', () => {
    const fs = newFS()
    const payload = fs.toDraftPayload([], '')
    const fs2 = new ChartFS()
    fs2.loadFromDraft(payload)
    const bin = fs2.get('bin/logo.png')!
    expect(bin.binary).toBe(true)
    expect(bin.size).toBe(1024)
  })
})
