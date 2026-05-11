# Copyright The Volcano Authors.
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

import unittest
from unittest.mock import MagicMock, patch

from kthena.downloader.base import get_downloader
from kthena.downloader.downloader import download_model
from kthena.downloader.modelscope_downloader import ModelScopeDownloader


class TestDownloadModel(unittest.TestCase):
    def setUp(self):
        self.source = "ms://Qwen/Qwen2.5-7B-Instruct"
        self.output_dir = "/tmp/models"
        self.credentials = {"ms_token": "fake-token"}
        self.mock_downloader = MagicMock()

    def tearDown(self):
        pass

    @patch("kthena.downloader.downloader.get_downloader")
    def test_download_model_invalid_credentials(self, mock_get_downloader):
        self.mock_downloader.download_model.side_effect = ValueError("Invalid authentication token")
        mock_get_downloader.return_value = self.mock_downloader

        with self.assertRaises(ValueError) as context:
            download_model(self.source, self.output_dir, self.credentials)

        self.assertIn("Invalid authentication token", str(context.exception))
        mock_get_downloader.assert_called_once_with(self.source, self.credentials, 8)

    @patch("kthena.downloader.downloader.get_downloader")
    def test_download_model_network_error(self, mock_get_downloader):
        self.mock_downloader.download_model.side_effect = ConnectionError(
            "Failed to establish connection to ModelScope server"
        )
        mock_get_downloader.return_value = self.mock_downloader

        with self.assertRaises(ConnectionError) as context:
            download_model(self.source, self.output_dir, self.credentials)

        self.assertIn("Failed to establish connection to ModelScope server", str(context.exception))
        mock_get_downloader.assert_called_once_with(self.source, self.credentials, 8)

    @patch("kthena.downloader.downloader.get_downloader")
    def test_download_model_permission_error(self, mock_get_downloader):
        self.mock_downloader.download_model.side_effect = PermissionError(
            "Insufficient permissions to write to output directory"
        )
        mock_get_downloader.return_value = self.mock_downloader

        with self.assertRaises(PermissionError) as context:
            download_model(self.source, self.output_dir, self.credentials)

        self.assertIn("Insufficient permissions", str(context.exception))

    def test_get_downloader_modelscope_basic(self):
        downloader = get_downloader(self.source, self.credentials)
        self.assertIsInstance(
            downloader,
            ModelScopeDownloader,
            "Should return ModelScopeDownloader instance",
        )
        if isinstance(downloader, ModelScopeDownloader):
            self.assertEqual(
                downloader.model_uri, "Qwen/Qwen2.5-7B-Instruct", "model_uri should have ms:// prefix removed"
            )

    def test_get_downloader_modelscope_with_revision(self):
        credentials_with_revision = {"ms_token": "fake-token", "ms_revision": "v1.0.0"}
        downloader = get_downloader(self.source, credentials_with_revision)
        self.assertIsInstance(
            downloader,
            ModelScopeDownloader,
            "Should return ModelScopeDownloader instance",
        )
        if isinstance(downloader, ModelScopeDownloader):
            self.assertEqual(downloader.ms_revision, "v1.0.0", "ms_revision should be passed correctly")

    @patch("kthena.downloader.modelscope_downloader.snapshot_download")
    def test_download_passes_token_to_snapshot_download(self, mock_snapshot_download):
        """Verify that ms_token is correctly passed to snapshot_download"""
        mock_snapshot_download.return_value = None
        downloader = ModelScopeDownloader(model_uri="Qwen/Qwen2.5-7B-Instruct", ms_token="test-token-123")
        downloader.download("/tmp/output")

        mock_snapshot_download.assert_called_once_with(
            model_id="Qwen/Qwen2.5-7B-Instruct",
            revision=None,
            local_dir="/tmp/output",
            local_files_only=False,
            token="test-token-123",
            max_workers=8,
        )

    @patch("kthena.downloader.modelscope_downloader.snapshot_download")
    def test_download_passes_all_params_to_snapshot_download(self, mock_snapshot_download):
        """Verify that all parameters are correctly passed to snapshot_download"""
        mock_snapshot_download.return_value = None
        downloader = ModelScopeDownloader(
            model_uri="Qwen/Qwen2.5-7B-Instruct", ms_revision="v1.0.0", ms_token="test-token", max_workers=4
        )
        downloader.download("/tmp/output")

        mock_snapshot_download.assert_called_once_with(
            model_id="Qwen/Qwen2.5-7B-Instruct",
            revision="v1.0.0",
            local_dir="/tmp/output",
            local_files_only=False,
            token="test-token",
            max_workers=4,
        )

    def test_get_downloader_modelscope_default_workers(self):
        """Verify that default max_workers (8) is passed to ModelScopeDownloader"""
        downloader = get_downloader(self.source, self.credentials)
        self.assertIsInstance(
            downloader,
            ModelScopeDownloader,
            "Should return ModelScopeDownloader instance",
        )
        if isinstance(downloader, ModelScopeDownloader):
            self.assertEqual(downloader.max_workers, 8, "max_workers should default to 8")

    def test_get_downloader_modelscope_custom_workers(self):
        """Verify that custom max_workers is passed to ModelScopeDownloader"""
        downloader = get_downloader(self.source, self.credentials, max_workers=16)
        self.assertIsInstance(
            downloader,
            ModelScopeDownloader,
            "Should return ModelScopeDownloader instance",
        )
        if isinstance(downloader, ModelScopeDownloader):
            self.assertEqual(downloader.max_workers, 16, "max_workers should be 16")


if __name__ == "__main__":
    unittest.main()
