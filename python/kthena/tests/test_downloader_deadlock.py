# Copyright The Volcano Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import logging
import subprocess
import sys
import threading
import unittest
from unittest.mock import patch

from kthena.downloader.pvc import PVCDownloader
from kthena.downloader.s3 import S3Downloader

# ~126KB on stderr -- twice the 64KB OS pipe buffer -- and a single stdout line
# written only after all of it. A parent that drains stdout to EOF before
# touching stderr can never reach that line: the child is blocked in write(2)
# on a full stderr pipe, and the parent is blocked in readline() on stdout.
NOISY = [
    sys.executable,
    "-c",
    "import sys\n"
    "for i in range(2000): sys.stderr.write('e'*60 + str(i) + '\\n')\n"
    "sys.stdout.write('done\\n')\n",
]

CASES = [
    ("s3", lambda: S3Downloader._execute_command(NOISY, None)),
    ("pvc", lambda: PVCDownloader._copy_from_pvc("/fake/src", "/fake/dst")),
]

TIMEOUT_SECONDS = 15


class TestDownloaderPipeDeadlock(unittest.TestCase):
    def setUp(self):
        logging.disable(logging.CRITICAL)
        self.addCleanup(logging.disable, logging.NOTSET)

    def test_large_child_stderr_does_not_deadlock(self):
        real = subprocess.Popen

        for name, run in CASES:
            with self.subTest(name):
                kids, errs = [], []

                def spawn(*a, **kw):
                    kids.append(real(NOISY, **kw))
                    return kids[-1]

                def target():
                    try:
                        run()
                    except BaseException as e:  # noqa: BLE001
                        errs.append(e)

                with patch("subprocess.Popen", side_effect=spawn), \
                     patch("pathlib.Path.exists", return_value=True), \
                     patch("pathlib.Path.is_dir", return_value=True), \
                     patch("pathlib.Path.mkdir"):
                    t = threading.Thread(target=target, daemon=True)
                    t.start()
                    t.join(timeout=TIMEOUT_SECONDS)
                    alive = t.is_alive()
                    for p in kids:  # frees both the child and the blocked reader
                        p.kill()
                        p.wait()
                    t.join(timeout=TIMEOUT_SECONDS)

                self.assertTrue(kids, f"{name} never spawned a child process")
                self.assertFalse(alive, f"{name} deadlocked on child stderr")
                self.assertEqual(errs, [], f"{name} raised: {errs}")


if __name__ == "__main__":
    unittest.main()
