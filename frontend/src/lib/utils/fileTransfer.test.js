import { describe, it, expect } from 'vitest';
import {
  autoRename,
  basename,
  carriesFiles,
  copyRequests,
  dirOf,
  dstPathsFor,
  existingOf,
  flattenDrop,
  isTerminalStatus,
  jobFileCount,
  jobPercent,
  joinPath,
  planDownload,
  planUploads,
  sortEntries,
  typeAheadIndex,
} from './fileTransfer.js';

describe('carriesFiles', () => {
  it('matches the server: ssh machines and the host card, nothing else', () => {
    expect(carriesFiles({ protocol: 'ssh' })).toBe(true);
    expect(carriesFiles({ protocol: 'local', builtin: true })).toBe(true);
    for (const p of ['telnet', 'serial', 'mosh']) {
      expect(carriesFiles({ protocol: p })).toBe(false);
    }
    // A local record nobody registered is not the host card, and a
    // builtin telnet/serial is not something the server will ever dial.
    expect(carriesFiles({ protocol: 'local' })).toBe(false);
    expect(carriesFiles({ protocol: 'serial', builtin: true })).toBe(false);
    expect(carriesFiles(null)).toBe(false);
    expect(carriesFiles({})).toBe(false);
  });

  it('is the filter the picker and the tab share', () => {
    const rows = [
      { id: 'a', protocol: 'ssh' },
      { id: 'b', protocol: 'telnet' },
      { id: 'host', protocol: 'local', builtin: true },
    ];
    expect(rows.filter(carriesFiles).map((r) => r.id)).toEqual(['a', 'host']);
  });
});

describe('path helpers', () => {
  it('roots joins at the filesystem root', () => {
    expect(joinPath('/', 'a')).toBe('/a');
    expect(joinPath('', 'a')).toBe('/a');
    expect(joinPath('/srv', 'a')).toBe('/srv/a');
  });

  it('keeps the root as the parent of a top-level entry', () => {
    expect(dirOf('/a')).toBe('/');
    expect(dirOf('/a/b')).toBe('/a');
    expect(basename('/a/b')).toBe('b');
    expect(basename('b')).toBe('b');
  });
});

describe('conflict probing', () => {
  it('builds one candidate path per selected entry', () => {
    expect(dstPathsFor([{ name: 'a' }, { name: 'b' }], '/srv')).toEqual(['/srv/a', '/srv/b']);
  });

  it('treats a dangling symlink as taken', () => {
    const stats = [{ path: '/srv/a', exists: true }, { path: '/srv/b', exists: false }, null];
    expect(existingOf(stats)).toEqual(['/srv/a']);
  });
});

describe('autoRename', () => {
  it('leaves a free name untouched and records it', () => {
    const taken = new Set();
    expect(autoRename('a.txt', taken)).toBe('a.txt');
    expect(taken.has('a.txt')).toBe(true);
  });

  it('inserts the counter before the extension', () => {
    const taken = new Set(['a.txt']);
    expect(autoRename('a.txt', taken)).toBe('a (1).txt');
  });

  it('accumulates names generated inside the same batch', () => {
    const taken = new Set(['deploy.sh']);
    expect([
      autoRename('deploy.sh', taken),
      autoRename('deploy.sh', taken),
      autoRename('deploy.sh', taken),
    ]).toEqual(['deploy (1).sh', 'deploy (2).sh', 'deploy (3).sh']);
  });

  it('handles extension-less and dot-only names', () => {
    expect(autoRename('app', new Set(['app']))).toBe('app (1)');
    expect(autoRename('.bashrc', new Set(['.bashrc']))).toBe('.bashrc (1)');
  });
});

describe('copyRequests', () => {
  it('asks for one job per entry and recurses only on directories', () => {
    const reqs = copyRequests(
      [
        { name: 'a', path: '/srv/a', is_dir: false },
        { name: 'd', path: '/srv/d', is_dir: true },
      ],
      'm2',
      '/dst',
      'rename'
    );
    expect(reqs).toEqual([
      {
        // Must mirror the server's JSON tag: `remote_path`. A `src_path`
        // key would decode to "" and the job would plan from `/`.
        remote_path: '/srv/a',
        dst_machine_id: 'm2',
        dst_dir: '/dst',
        recursive: false,
        overwrite: 'rename',
      },
      {
        remote_path: '/srv/d',
        dst_machine_id: 'm2',
        dst_dir: '/dst',
        recursive: true,
        overwrite: 'rename',
      },
    ]);
  });
});

describe('planUploads', () => {
  it('maps a flat drop onto the destination directory', () => {
    const { dirs, files } = planUploads([{ file: { name: 'a' }, relPath: 'a.txt' }], '/home/u');
    expect(dirs).toEqual([]);
    expect(files).toEqual([{ file: { name: 'a' }, remotePath: '/home/u/a.txt' }]);
  });

  it('recreates the folder skeleton, parents first', () => {
    const { dirs, files } = planUploads(
      [{ dir: 'p' }, { dir: 'p/sub' }, { file: { name: 'x' }, relPath: 'p/sub/deep/x.txt' }],
      '/dst'
    );
    expect(dirs).toEqual(['/dst/p', '/dst/p/sub', '/dst/p/sub/deep']);
    expect(files).toEqual([{ file: { name: 'x' }, remotePath: '/dst/p/sub/deep/x.txt' }]);
  });

  it('resolves same-batch collisions against each other', () => {
    const { files } = planUploads(
      [
        { file: {}, relPath: 'a/a.txt' },
        { file: {}, relPath: 'b/a.txt' },
      ],
      '/dst',
      new Set(['a.txt'])
    );
    expect(files.map((f) => f.remotePath)).toEqual(['/dst/a/a (1).txt', '/dst/b/a (2).txt']);
  });

  it('plans nothing for an empty drop', () => {
    expect(planUploads([], '/dst')).toEqual({ dirs: [], files: [] });
  });
});

describe('sortEntries', () => {
  const entries = [
    { name: 'b.txt', is_dir: false, size: 5, mod_time: 10 },
    { name: 'A', is_dir: true, size: 0, mod_time: 20 },
    { name: '.hidden', is_dir: false, size: 1, mod_time: 30 },
  ];

  it('hides dotfiles unless asked', () => {
    expect(sortEntries(entries).map((e) => e.name)).toEqual(['A', 'b.txt']);
    expect(sortEntries(entries, { showHidden: true }).map((e) => e.name)).toEqual([
      'A',
      '.hidden',
      'b.txt',
    ]);
  });

  it('keeps directories on top for a size sort', () => {
    expect(sortEntries(entries, { sort: 'size', showHidden: true }).map((e) => e.name)).toEqual([
      'A',
      '.hidden',
      'b.txt',
    ]);
  });

  it('sorts by ascending size and newest modification when told not to group', () => {
    const flat = { showHidden: true, dirsFirst: false };
    expect(sortEntries(entries, { ...flat, sort: 'size' }).map((e) => e.name)).toEqual([
      'A',
      '.hidden',
      'b.txt',
    ]);
    expect(sortEntries(entries, { ...flat, sort: 'time' }).map((e) => e.name)).toEqual([
      '.hidden',
      'A',
      'b.txt',
    ]);
  });
});

describe('typeAheadIndex', () => {
  const names = ['etc', 'home', 'httpd', 'var'];

  it('finds the first match from the top', () => {
    expect(typeAheadIndex(names, 'h')).toBe(1);
  });

  it('advances on the next press and wraps once', () => {
    expect(typeAheadIndex(names, 'h', 1)).toBe(2);
    expect(typeAheadIndex(names, 'h', 2)).toBe(1);
  });

  it('reports -1 for a buffer that matches nothing', () => {
    expect(typeAheadIndex(names, 'z')).toBe(-1);
    expect(typeAheadIndex(names, '')).toBe(-1);
    expect(typeAheadIndex([], 'h')).toBe(-1);
  });
});

describe('queue math', () => {
  it('returns null when no total is known yet', () => {
    expect(jobPercent({ status: 'running', done: 10, total: 0 })).toBeNull();
    expect(jobPercent(null)).toBeNull();
  });

  it('caps an in-flight job below 100 and completes a done one', () => {
    expect(jobPercent({ status: 'running', done: 50, total: 100 })).toBe(50);
    expect(jobPercent({ status: 'running', done: 100, total: 100 })).toBe(99);
    expect(jobPercent({ status: 'done', done: 3, total: 100 })).toBe(100);
  });

  it('treats a single-file job as one file', () => {
    expect(jobFileCount({ files_done: 4, files_total: 20 })).toEqual({ done: 4, total: 20 });
    expect(jobFileCount({ files_done: 0 })).toEqual({ done: 0, total: 1 });
  });

  it('recognises finished statuses', () => {
    expect(['done', 'error', 'canceled'].map(isTerminalStatus)).toEqual([true, true, true]);
    expect(isTerminalStatus('running')).toBe(false);
  });
});

describe('flattenDrop', () => {
  function dirEntry(name, children) {
    return {
      name,
      isDirectory: true,
      isFile: false,
      createReader() {
        let sent = false;
        return {
          readEntries: (ok) => {
            // One call per 100-entry page, terminated by an empty batch.
            const batch = sent ? [] : children;
            sent = true;
            ok(batch);
          },
        };
      },
    };
  }
  function fileEntry(name, body = {}) {
    return { name, isFile: true, isDirectory: false, file: (ok) => ok(body) };
  }

  it('walks nested folders across readEntries pages', async () => {
    const root = dirEntry('proj', [dirEntry('src', [fileEntry('a.c', { n: 1 })])]);
    const dt = { items: [{ webkitGetAsEntry: () => root }], files: [] };
    expect(await flattenDrop(dt)).toEqual([
      { dir: 'proj' },
      { dir: 'proj/src' },
      { file: { n: 1 }, relPath: 'proj/src/a.c' },
    ]);
  });

  it('falls back to the files list when the entry API is missing', async () => {
    const dt = { items: [{ webkitGetAsEntry: () => null }], files: [{ name: 'a.txt' }] };
    expect(await flattenDrop(dt)).toEqual([{ file: { name: 'a.txt' }, relPath: 'a.txt' }]);
  });

  it('survives an empty DataTransfer', async () => {
    expect(await flattenDrop(undefined)).toEqual([]);
  });
});

describe('planDownload', () => {
  it('keeps a single entry on the legacy single-file path', () => {
    expect(planDownload([{ path: '/a/b.txt', name: 'b.txt', is_dir: false }])).toEqual({
      mode: 'single',
      path: '/a/b.txt',
      file_name: 'b.txt',
    });
  });

  it('merges several entries into one batch, order preserved', () => {
    expect(
      planDownload([
        { path: '/logs', name: 'logs', is_dir: true },
        { path: '/notes.txt', name: 'notes.txt', is_dir: false },
      ])
    ).toEqual({ mode: 'batch', paths: ['/logs', '/notes.txt'] });
  });

  it('plans nothing when there is no selection', () => {
    expect(planDownload([])).toBeNull();
    expect(planDownload(undefined)).toBeNull();
  });
});
