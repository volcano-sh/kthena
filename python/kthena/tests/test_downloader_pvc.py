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

import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

from kthena.downloader.base import get_downloader
from kthena.downloader.downloader import download_model
from kthena.downloader.pvc import PVCDownloader
from kthena.downloader.lock import LockManager


class TestDownloadModel(unittest.TestCase):
    def setUp(self):
        self.source = "pvc://models"
        self.output_dir = "/tmp/models"
        self.config = {}
        self.mock_downloader = MagicMock()

    def tearDown(self):
        pass

    @patch("kthena.downloader.downloader.get_downloader")
    def test_download_model_pvc_default(self, mock_get_downloader):
        mock_get_downloader.return_value = self.mock_downloader

        download_model(self.source, self.output_dir, self.config)

        mock_get_downloader.assert_called_once_with(self.source, self.config, 8)
        self.mock_downloader.download_model.assert_called_once_with(self.output_dir)

    @patch("kthena.downloader.downloader.get_downloader")
    def test_download_model_pvc_custom_workers(self, mock_get_downloader):
        mock_get_downloader.return_value = self.mock_downloader

        custom_max_workers = 16
        download_model(
            self.source, self.output_dir, self.config, max_workers=custom_max_workers
        )

        mock_get_downloader.assert_called_once_with(
            self.source, self.config, custom_max_workers
        )
        self.mock_downloader.download_model.assert_called_once_with(self.output_dir)

    @patch("kthena.downloader.downloader.get_downloader")
    def test_download_model_file_error(self, mock_get_downloader):
        self.mock_downloader.download_model.side_effect = FileNotFoundError(
            "PVC path does not exist"
        )
        mock_get_downloader.return_value = self.mock_downloader

        with self.assertRaises(FileNotFoundError) as context:
            download_model(self.source, self.output_dir, self.config)

        self.assertIn("PVC path does not exist", str(context.exception))
        mock_get_downloader.assert_called_once_with(self.source, self.config, 8)

    @patch("kthena.downloader.downloader.get_downloader")
    def test_download_model_permission_error(self, mock_get_downloader):
        self.mock_downloader.download_model.side_effect = PermissionError(
            "Insufficient permissions to write to output directory"
        )
        mock_get_downloader.return_value = self.mock_downloader

        with self.assertRaises(PermissionError) as context:
            download_model(self.source, self.output_dir, self.config)

        self.assertIn("Insufficient permissions", str(context.exception))
        mock_get_downloader.assert_called_once_with(self.source, self.config, 8)

    def test_get_downloader_pvc(self):
        source = "pvc://models"
        downloader = get_downloader(source, {})
        self.assertIsInstance(downloader, PVCDownloader)
        self.assertEqual(downloader.source_path, source)

    def test_pvc_copy_preserves_active_destination_lock(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            source = Path(temp_dir) / "source"
            destination = Path(temp_dir) / "destination"
            source.mkdir()
            (source / ".lock").touch()
            # Different mtimes make rsync transfer even an empty persistent marker.
            os.utime(source / ".lock", (1, 1))
            (source / "weights.bin").write_bytes(b"model weights")
            (source / "nested").mkdir()
            (source / "nested" / ".lock").write_text("model data")
            downloader = PVCDownloader(source_path=f"pvc://{source}")
            original_copy = downloader._copy_from_pvc

            def copy_and_check_lock(pvc_path, output_dir):
                lock_path = destination / ".lock"
                original_inode = lock_path.stat().st_ino
                original_copy(pvc_path, output_dir)
                contender = LockManager(str(lock_path))
                try:
                    self.assertFalse(contender.try_acquire(), "Copy must not unlock the destination")
                    self.assertEqual(original_inode, lock_path.stat().st_ino)
                finally:
                    contender.release()

            with patch.object(downloader, "_copy_from_pvc", side_effect=copy_and_check_lock):
                downloader.download_model(str(destination))

            self.assertEqual(b"model weights", (destination / "weights.bin").read_bytes())
            self.assertEqual("model data", (destination / "nested" / ".lock").read_text())
            with LockManager(str(destination / ".lock")):
                pass

    def test_init_with_valid_source(self):
        downloader = PVCDownloader(source_path="pvc://models")
        self.assertEqual(downloader.source_path, "pvc://models")

    def test_init_with_empty_source(self):
        with self.assertRaises(ValueError) as context:
            PVCDownloader(source_path=None)
        self.assertIn("PVC source URI must be provided", str(context.exception))

    def test_parse_pvc_path(self):
        test_cases = [
            ("pvc://models", "/models"),
            ("pvc://data/models", "/data/models"),
        ]

        for source, expected in test_cases:
            downloader = PVCDownloader(source_path=source)
            self.assertEqual(downloader._parse_pvc_path(), expected)

    def test_parse_pvc_path_empty(self):
        downloader = PVCDownloader(source_path="pvc://")
        with self.assertRaises(ValueError) as context:
            downloader._parse_pvc_path()
        self.assertIn("No path specified in PVC URI", str(context.exception))

    @patch("subprocess.Popen")
    @patch("os.path.exists")
    @patch("pathlib.Path.exists")
    @patch("pathlib.Path.is_dir")
    @patch("pathlib.Path.mkdir")
    def test_copy_from_pvc_success(
        self, _, mock_is_dir, mock_exists, mock_os_exists, mock_popen
    ):
        mock_is_dir.return_value = True
        mock_exists.return_value = True
        mock_os_exists.return_value = True

        mock_process = MagicMock()
        mock_process.stdout.readline.side_effect = [
            "Sending file1.txt",
            "Sending file2.txt",
            "",
        ]
        mock_process.wait.return_value = 0
        mock_popen.return_value = mock_process

        result = PVCDownloader._copy_from_pvc("/fake/source", "/fake/dest")

        self.assertTrue(result)
        mock_popen.assert_called_once()
        cmd_args = mock_popen.call_args[0][0]
        self.assertIn("rsync", cmd_args[0])
        self.assertIn("--progress", cmd_args)

    @patch("subprocess.Popen")
    @patch("os.path.exists")
    @patch("pathlib.Path.exists")
    @patch("pathlib.Path.is_dir")
    @patch("pathlib.Path.mkdir")
    def test_copy_from_pvc_failure(
        self, _, mock_is_dir, mock_exists, mock_os_exists, mock_popen
    ):
        mock_is_dir.return_value = True
        mock_exists.return_value = True
        mock_os_exists.return_value = True

        mock_process = MagicMock()
        mock_process.stdout.readline.side_effect = ["Starting transfer", ""]
        mock_process.wait.return_value = 1
        mock_process.stderr.read.return_value = "Error: Permission denied"
        mock_popen.return_value = mock_process

        with self.assertRaises(subprocess.SubprocessError):
            PVCDownloader._copy_from_pvc("/fake/source", "/fake/dest")

    @patch("os.path.exists")
    @patch("pathlib.Path.exists")
    def test_copy_from_nonexistent_path(self, mock_path_exists, mock_os_exists):
        mock_path_exists.return_value = False
        mock_os_exists.return_value = False

        with self.assertRaises(FileNotFoundError):
            PVCDownloader._copy_from_pvc("/nonexistent/path", "/fake/dest")

    @patch("kthena.downloader.pvc.PVCDownloader._copy_from_pvc")
    @patch("kthena.downloader.pvc.PVCDownloader._parse_pvc_path")
    @patch("os.makedirs")
    def test_download_success(self, _, mock_parse_path, mock_copy):
        mock_parse_path.return_value = "/parsed/pvc/path"
        mock_copy.return_value = True

        downloader = PVCDownloader(source_path="pvc://test")
        downloader.download("/output/dir")

        mock_parse_path.assert_called_once()
        mock_copy.assert_called_once_with("/parsed/pvc/path", "/output/dir")

    @patch("kthena.downloader.pvc.PVCDownloader._parse_pvc_path")
    def test_download_path_error(self, mock_parse_path):
        mock_parse_path.side_effect = ValueError("Invalid path")

        downloader = PVCDownloader(source_path="pvc://test")
        with self.assertRaises(ValueError):
            downloader.download("/output/dir")


if __name__ == "__main__":
    unittest.main()
