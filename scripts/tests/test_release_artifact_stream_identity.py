"""Finite filesystem race witnesses, not publication or future-byte authority."""
from contextlib import contextmanager
import errno
import hashlib
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import release_artifact_manifest as manifest


def windows_fixture_handle(path, access):
    import ctypes
    from ctypes import wintypes

    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    create = kernel.CreateFileW
    create.argtypes = [wintypes.LPCWSTR, wintypes.DWORD, wintypes.DWORD, wintypes.LPVOID,
                       wintypes.DWORD, wintypes.DWORD, wintypes.HANDLE]
    create.restype = wintypes.HANDLE
    close = kernel.CloseHandle
    close.argtypes, close.restype = [wintypes.HANDLE], wintypes.BOOL
    handle = create(str(path), access, 1 | 2 | 4, None, 3, 0x80 | 0x00200000, None)
    if handle == wintypes.HANDLE(-1).value:
        raise ctypes.WinError(ctypes.get_last_error())
    return kernel, handle


def shared_delete_reader(path, flags):
    """Test-only Windows sharing lets a real replacement reach the identity check."""
    import msvcrt

    kernel, handle = windows_fixture_handle(path, 0x80000000)
    try:
        return msvcrt.open_osfhandle(handle, flags)
    except BaseException:
        kernel.CloseHandle(handle)
        raise


def replace_during_read(source, target):
    if os.name != "nt":
        return os.replace(source, target)
    import ctypes
    from ctypes import wintypes

    class RenameInfo(ctypes.Structure):
        _fields_ = [("Flags", wintypes.DWORD), ("RootDirectory", wintypes.HANDLE),
                    ("FileNameLength", wintypes.DWORD), ("FileName", wintypes.WCHAR * 1)]

    # FileRenameInfoEx + REPLACE_IF_EXISTS/POSIX_SEMANTICS exercises an actual
    # replacement even with open handles; MoveFileEx (os.replace) refuses it.
    # https://learn.microsoft.com/windows-hardware/drivers/ddi/ntifs/ns-ntifs-_file_rename_information
    name = str(target).encode("utf-16-le")
    buffer = ctypes.create_string_buffer(RenameInfo.FileName.offset + len(name) + 2)
    info = ctypes.cast(buffer, ctypes.POINTER(RenameInfo)).contents
    info.Flags, info.RootDirectory, info.FileNameLength = 1 | 2, None, len(name)
    ctypes.memmove(ctypes.addressof(buffer) + RenameInfo.FileName.offset, name, len(name))
    kernel, handle = windows_fixture_handle(source, 0x00010000)
    rename = kernel.SetFileInformationByHandle
    rename.argtypes, rename.restype = [wintypes.HANDLE, ctypes.c_int, wintypes.LPVOID, wintypes.DWORD], wintypes.BOOL
    try:
        if not rename(handle, 22, buffer, len(buffer)):
            raise ctypes.WinError(ctypes.get_last_error())
    finally:
        kernel.CloseHandle(handle)


class MutatingReader:
    def __init__(self, stream, mutate):
        self.stream, self.mutate = stream, mutate
        self.changed = False

    def __enter__(self):
        self.stream.__enter__()
        return self

    def __exit__(self, *arguments):
        return self.stream.__exit__(*arguments)

    def __getattr__(self, name):
        return getattr(self.stream, name)

    def read(self, size=-1):
        value = self.stream.read(size)
        if not self.changed:
            self.changed = True
            self.mutate()
        return value


class ReleaseArtifactStreamIdentityTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.asset = self.root / "candidate.msi"
        self.original = b"original release bytes\r\n" * 16
        self.asset.write_bytes(self.original)

    @contextmanager
    def mutate_during_read(self, mutate):
        path_open, fdopen, native_open = Path.open, os.fdopen, manifest._open_read_descriptor
        completed = []

        def recorded_mutation():
            mutate()
            completed.append(True)

        def path_reader(path, *arguments, **keywords):
            stream = path_open(path, *arguments, **keywords)
            return MutatingReader(stream, recorded_mutation) if path == self.asset and arguments == ("rb",) else stream

        with mock.patch.object(Path, "open", path_reader), \
             mock.patch.object(manifest, "_open_read_descriptor", side_effect=shared_delete_reader if os.name == "nt" else native_open), \
             mock.patch.object(os, "fdopen", side_effect=lambda *a, **kw: MutatingReader(fdopen(*a, **kw), recorded_mutation)):
            yield
        self.assertEqual(completed, [True], "fixture mutation itself must complete; an I/O error is not rejection proof")

    def reject(self):
        with self.assertRaisesRegex(manifest.ReleaseMetadataError, "release artifact"):
            manifest.sha256_file(self.asset)

    def test_stable_binary_content_is_hashed_once_without_text_translation(self):
        with mock.patch.object(manifest, "_open_read_descriptor", wraps=manifest._open_read_descriptor) as opened:
            result = manifest.sha256_file(self.asset)
        self.assertEqual(result, hashlib.sha256(self.original).hexdigest())
        self.assertEqual(opened.call_count, 1)
        if os.name == "nt":
            with self.asset.open("r+b"):
                self.reject()

    def test_stable_executable_suffixes_are_not_cross_api_mode_changes(self):
        for suffix in (".exe", ".bat", ".cmd", ".com"):
            with self.subTest(suffix=suffix):
                artifact = self.root / ("candidate" + suffix)
                artifact.write_bytes(self.original)
                self.assertEqual(manifest.sha256_file(artifact), hashlib.sha256(self.original).hexdigest())

    def test_previously_modified_stable_file_is_not_a_cross_api_ctime_mismatch(self):
        content = self.original + b"completed earlier modification"
        if os.name == "nt":
            import ctypes
            from ctypes import wintypes

            kernel, handle = windows_fixture_handle(self.asset, 0x00000100)
            set_time = kernel.SetFileTime
            set_time.argtypes = [wintypes.HANDLE, ctypes.POINTER(wintypes.FILETIME),
                                 ctypes.POINTER(wintypes.FILETIME), ctypes.POINTER(wintypes.FILETIME)]
            set_time.restype = wintypes.BOOL
            ticks = 116444736000000000
            creation = wintypes.FILETIME(ticks & 0xffffffff, ticks >> 32)
            try:
                if not set_time(handle, ctypes.byref(creation), None, None):
                    raise ctypes.WinError(ctypes.get_last_error())
            finally:
                kernel.CloseHandle(handle)
        self.asset.write_bytes(content)
        self.assertEqual(manifest.sha256_file(self.asset), hashlib.sha256(content).hexdigest())

    def test_native_observation_failure_never_returns_a_digest(self):
        with mock.patch.object(manifest, "_descriptor_change_time", side_effect=OSError("unavailable")):
            self.reject()
        if os.name == "nt":
            import ctypes
            from types import SimpleNamespace

            descriptor = manifest._open_read_descriptor(self.asset, os.O_RDONLY | os.O_BINARY)
            try:
                for result in (0, 1):
                    def query(*_arguments):
                        return result
                    with mock.patch.object(ctypes, "WinDLL", return_value=SimpleNamespace(GetFileInformationByHandleEx=query)):
                        with self.assertRaises((OSError, manifest.ReleaseMetadataError)):
                            manifest._descriptor_change_time(descriptor)
            finally:
                os.close(descriptor)

    def test_in_place_rewrite_with_restored_mtime_is_rejected(self):
        original = self.asset.stat()
        if os.name == "nt":
            blocked = []

            def attempted_write():
                with self.assertRaises(OSError) as native_refusal:
                    kernel, handle = windows_fixture_handle(self.asset, 0x40000000)
                    kernel.CloseHandle(handle)
                self.assertEqual(native_refusal.exception.winerror, 32, "require a native sharing violation")
                with self.assertRaises(PermissionError) as refusal:
                    self.asset.write_bytes(b"X" * len(self.original))
                self.assertEqual(refusal.exception.errno, errno.EACCES, "CRT maps the sharing violation to EACCES")
                blocked.append(True)

            fdopen = os.fdopen
            with mock.patch.object(os, "fdopen", side_effect=lambda *a, **kw: MutatingReader(fdopen(*a, **kw), attempted_write)):
                self.assertEqual(manifest.sha256_file(self.asset), hashlib.sha256(self.original).hexdigest())
            self.assertEqual(blocked, [True])
            self.assertEqual(self.asset.read_bytes(), self.original)
            return

        def mutate():
            self.asset.write_bytes(b"X" * len(self.original))
            os.utime(self.asset, ns=(original.st_atime_ns, original.st_mtime_ns))

        with self.mutate_during_read(mutate):
            self.reject()

    def test_atomic_replacement_with_identical_bytes_is_rejected(self):
        replacement = self.root / "replacement"
        replacement.write_bytes(self.original)
        with self.mutate_during_read(lambda: replace_during_read(replacement, self.asset)):
            self.reject()

    def test_growth_after_open_is_rejected_instead_of_accepting_a_mixed_stream(self):
        with self.mutate_during_read(lambda: self.asset.write_bytes(self.original + b"more")):
            self.reject()

    def test_path_disappearing_after_read_is_rejected(self):
        with self.mutate_during_read(self.asset.unlink):
            self.reject()

    def test_symlink_is_rejected_before_open(self):
        target = self.root / "target"
        self.asset.rename(target)
        self.asset.symlink_to(target)
        with mock.patch.object(manifest, "_open_read_descriptor", side_effect=AssertionError("symlink was opened")):
            self.reject()

    @unittest.skipUnless(hasattr(os, "mkfifo"), "POSIX FIFO witness")
    def test_fifo_is_rejected_before_any_potentially_blocking_open(self):
        self.asset.unlink()
        os.mkfifo(self.asset)
        with mock.patch.object(Path, "open", side_effect=AssertionError("FIFO was opened")), \
             mock.patch.object(os, "open", side_effect=AssertionError("FIFO was opened")):
            self.reject()

    def test_preopen_identity_substitution_is_rejected(self):
        replacement = self.root / "replacement"
        replacement.write_bytes(self.original)
        original_open = manifest._open_read_descriptor

        def swapped(*arguments, **keywords):
            os.replace(replacement, self.asset)
            return original_open(*arguments, **keywords)

        with mock.patch.object(manifest, "_open_read_descriptor", side_effect=swapped):
            self.reject()

    def test_portable_fallback_still_checks_identity_without_posix_open_flags(self):
        # Exercise the portable path without claiming a native Windows run.
        with mock.patch.object(os, "O_NOFOLLOW", 0, create=True), \
             mock.patch.object(os, "O_NONBLOCK", 0, create=True):
            self.assertEqual(manifest.sha256_file(self.asset), hashlib.sha256(self.original).hexdigest())
            replacement = self.root / "replacement"
            replacement.write_bytes(self.original)
            with self.mutate_during_read(lambda: replace_during_read(replacement, self.asset)):
                self.reject()


if __name__ == "__main__":
    unittest.main()
