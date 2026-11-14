#Requires -Version 7.2
[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [ValidateSet('Snapshot', 'Broaden', 'Junction', 'RemoveJunction', 'WrongOwner', 'Handles')]
    [string] $Action,
    [string] $Path,
    [string] $Target,
    [uint32] $ActorPid
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if (-not $IsWindows) { throw 'This fixture requires Windows and PowerShell 7.' }

# The launcher copies this entire file into a newly created private namespace;
# the Go tests bind its bytes by SHA-256 before every invocation. Native calls
# use retained handles, including for reparse data and security descriptors.
Add-Type -TypeDefinition @'
using System;
using System.Collections.Generic;
using System.ComponentModel;
using System.IO;
using System.Runtime.InteropServices;
using System.Security.AccessControl;
using System.Security.Cryptography;
using System.Security.Principal;
using System.Text;
using Microsoft.Win32.SafeHandles;

namespace RecoveryNamespaceFixture {
    public sealed class Pin : IDisposable {
        internal readonly List<SafeFileHandle> Handles = new List<SafeFileHandle>();
        public SafeFileHandle Leaf;
        public string Path;
        public void Dispose() { for (int i = Handles.Count - 1; i >= 0; --i) Handles[i].Dispose(); }
    }
    public sealed class Node {
        public string Name;
        public bool Exists;
        public string Identity;
        public uint Attributes;
        public uint Links;
        public ulong CreationTime;
        public ulong LastWriteTime;
        public ulong Size;
        public string Security;
        public string ContentSHA256;
        public string ReparseData;
        public Node[] Entries;
    }
    public sealed class ActorObservation {
        public uint ActorPid;
        public ulong CreationTimeFILETIME;
        public string ExecutablePath;
        public uint HandleCount;
    }
    public sealed class OwnerAttempt {
        public bool Assigned;
        public string Failure;
        public int Attempts;
        public string OwnerBefore;
        public string OwnerAfter;
        public string RequestedOwner;
    }
    public sealed class Scope : IDisposable {
        const uint ReadControl = 0x00020000, ReadAttributes = 0x80, GenericRead = 0x80000000;
        const uint WriteDac = 0x00040000, WriteOwner = 0x00080000, Delete = 0x00010000;
        const uint DirectoryAttribute = 0x10, ReparseAttribute = 0x400, MountPointTag = 0xa0000003;
        const uint GetReparse = 0x000900a8, SetReparse = 0x000900a4;
        readonly string root;
        readonly string temp;
        readonly Pin rootPin;
        int visited;

        [StructLayout(LayoutKind.Sequential)] struct FT { public uint Low, High; public ulong Value { get { return ((ulong)High << 32) | Low; } } }
        [StructLayout(LayoutKind.Sequential)] struct Info {
            public uint Attributes; public FT Created, Accessed, Written;
            public uint Volume, SizeHigh, SizeLow, Links, IndexHigh, IndexLow;
        }
        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        static extern SafeFileHandle CreateFileW(string path, uint access, uint share, IntPtr security, uint disposition, uint flags, IntPtr template);
        [DllImport("kernel32.dll", SetLastError = true)] static extern bool GetFileInformationByHandle(SafeFileHandle handle, out Info info);
        [DllImport("kernel32.dll", SetLastError = true)] static extern bool GetFileInformationByHandleEx(SafeFileHandle handle, int kind, [Out] byte[] data, uint length);
        [DllImport("kernel32.dll", SetLastError = true)] static extern bool SetFileInformationByHandle(SafeFileHandle handle, int kind, byte[] data, uint length);
        [DllImport("kernel32.dll", SetLastError = true)] static extern bool DeviceIoControl(SafeFileHandle handle, uint code, byte[] input, uint inputSize, [Out] byte[] output, uint outputSize, out uint returned, IntPtr overlapped);
        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)] static extern bool CreateDirectoryW(string path, IntPtr security);
        [DllImport("advapi32.dll")] static extern uint GetSecurityInfo(SafeFileHandle handle, uint kind, uint flags, out IntPtr owner, out IntPtr group, out IntPtr dacl, out IntPtr sacl, out IntPtr descriptor);
        [DllImport("advapi32.dll")] static extern uint GetSecurityDescriptorLength(IntPtr descriptor);
        [DllImport("advapi32.dll")] static extern uint SetSecurityInfo(SafeFileHandle handle, uint kind, uint flags, byte[] owner, IntPtr group, byte[] dacl, IntPtr sacl);
        [DllImport("kernel32.dll")] static extern IntPtr LocalFree(IntPtr value);
        [DllImport("kernel32.dll", SetLastError = true)] static extern SafeProcessHandle OpenProcess(uint access, bool inherit, uint pid);
        [DllImport("kernel32.dll", SetLastError = true)] static extern bool GetProcessHandleCount(SafeProcessHandle handle, out uint count);
        [DllImport("kernel32.dll", SetLastError = true)] static extern bool GetProcessTimes(SafeProcessHandle handle, out FT created, out FT exited, out FT kernel, out FT user);
        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)] static extern bool QueryFullProcessImageNameW(SafeProcessHandle handle, uint flags, StringBuilder name, ref uint length);
        [DllImport("kernel32.dll", SetLastError = true)] static extern uint WaitForSingleObject(SafeProcessHandle handle, uint milliseconds);

        static Exception LastError(string operation) { return new Win32Exception(Marshal.GetLastWin32Error(), operation); }
        static string Hex(byte[] bytes) { return BitConverter.ToString(bytes).Replace("-", "").ToLowerInvariant(); }
        static string CurrentUser() { using (var identity = WindowsIdentity.GetCurrent()) return identity.User.Value; }
        public static string Canonical(string value) {
            if (String.IsNullOrEmpty(value) || value.Length < 3 || !Char.IsLetter(value[0]) || value[1] != ':' || value[2] != '\\')
                throw new IOException("An absolute local drive path is required.");
            string full = System.IO.Path.GetFullPath(value).TrimEnd('\\');
            if (full.Length == 2) full += "\\";
            foreach (string part in full.Substring(3).Split('\\')) {
                if (part.Length == 0) continue;
                if (part.EndsWith(".") || part.EndsWith(" ") || part.IndexOfAny(System.IO.Path.GetInvalidFileNameChars()) >= 0)
                    throw new IOException("Ambiguous path component refused.");
                string stem = part.Split('.')[0].ToUpperInvariant();
                if (stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" ||
                    (stem.Length == 4 && (stem.StartsWith("COM") || stem.StartsWith("LPT")) && "123456789¹²³".IndexOf(stem[3]) >= 0))
                    throw new IOException("Device path refused.");
            }
            return full;
        }
        static Info Information(SafeFileHandle handle) {
            Info result; if (!GetFileInformationByHandle(handle, out result)) throw LastError("GetFileInformationByHandle"); return result;
        }
        static RawSecurityDescriptor Security(SafeFileHandle handle) {
            IntPtr owner, group, dacl, sacl, descriptor;
            uint error = GetSecurityInfo(handle, 1, 7, out owner, out group, out dacl, out sacl, out descriptor);
            if (error != 0) throw new Win32Exception((int)error, "GetSecurityInfo");
            try {
                uint length = GetSecurityDescriptorLength(descriptor);
                if (length == 0 || length > 1024 * 1024) throw new IOException("Invalid security descriptor size.");
                byte[] bytes = new byte[length]; Marshal.Copy(descriptor, bytes, 0, bytes.Length);
                return new RawSecurityDescriptor(bytes, 0);
            } finally { LocalFree(descriptor); }
        }
        static Pin PinPath(string path, uint leafAccess, bool missing) {
            Pin pin = new Pin { Path = Canonical(path) };
            try {
                string current = pin.Path.Substring(0, 3);
                var components = new List<string> { current };
                foreach (string part in pin.Path.Substring(3).Split('\\')) {
                    if (part.Length == 0) continue;
                    current = System.IO.Path.Combine(current, part); components.Add(current);
                }
                for (int i = 0; i < components.Count; ++i) {
                    bool leaf = i == components.Count - 1;
                    var handle = CreateFileW(components[i], (leaf ? leafAccess : ReadAttributes) | ReadControl,
                        1U, IntPtr.Zero, 3, 0x02200000, IntPtr.Zero);
                    if (handle.IsInvalid) {
                        int error = Marshal.GetLastWin32Error(); handle.Dispose();
                        if (leaf && missing && (error == 2 || error == 3)) return pin;
                        throw new Win32Exception(error, "Open no-follow fixture path");
                    }
                    pin.Handles.Add(handle);
                    var info = Information(handle);
                    if (!leaf && ((info.Attributes & DirectoryAttribute) == 0 || (info.Attributes & ReparseAttribute) != 0))
                        throw new IOException("Fixture ancestor is not an ordinary directory.");
                    if (leaf) pin.Leaf = handle;
                }
                return pin;
            } catch { pin.Dispose(); throw; }
        }
        public Scope(string fixtureRoot) {
            root = Canonical(fixtureRoot); temp = root + "\\temp\\";
            rootPin = PinPath(root, ReadAttributes, false);
            try {
                Info info = Information(rootPin.Leaf);
                if ((info.Attributes & (DirectoryAttribute | ReparseAttribute)) != DirectoryAttribute)
                    throw new IOException("Private creator root is not ordinary.");
                var sd = Security(rootPin.Leaf);
                string user = CurrentUser();
                if (sd.Owner == null || sd.Owner.Value != user || sd.DiscretionaryAcl == null ||
                    (sd.ControlFlags & ControlFlags.DiscretionaryAclProtected) == 0)
                    throw new IOException("Private creator root owner/protection refused.");
                bool ownerFull = false;
                foreach (GenericAce raw in sd.DiscretionaryAcl) {
                    CommonAce ace = raw as CommonAce;
                    if (ace == null || ace.IsCallback || ace.AceQualifier != AceQualifier.AccessAllowed ||
                        (ace.SecurityIdentifier.Value != user && ace.SecurityIdentifier.Value != "S-1-5-18" && ace.SecurityIdentifier.Value != "S-1-5-32-544"))
                        throw new IOException("Private creator root contains a foreign access grant.");
                    if (ace.SecurityIdentifier.Value == user && (ace.AccessMask & 0x1f01ff) == 0x1f01ff) ownerFull = true;
                }
                if (!ownerFull) throw new IOException("Creator root has no full owner access.");
            } catch { rootPin.Dispose(); throw; }
        }
        public void Dispose() { rootPin.Dispose(); }
        string TempPath(string path) {
            string full = Canonical(path);
            if (!full.StartsWith(temp, StringComparison.OrdinalIgnoreCase) || full.Length <= temp.Length)
                throw new IOException("Fixture action escaped the private temp namespace.");
            return full;
        }
        static void OrdinaryDirectory(Pin pin) {
            if (pin.Leaf == null || (Information(pin.Leaf).Attributes & (DirectoryAttribute | ReparseAttribute)) != DirectoryAttribute)
                throw new IOException("Ordinary fixture directory required.");
        }
        static byte[] ReparseBytes(SafeFileHandle handle) {
            byte[] data = new byte[16384]; uint returned;
            if (!DeviceIoControl(handle, GetReparse, null, 0, data, (uint)data.Length, out returned, IntPtr.Zero)) throw LastError("FSCTL_GET_REPARSE_POINT");
            if (returned < 8 || returned > data.Length) throw new IOException("Invalid reparse buffer size.");
            Array.Resize(ref data, (int)returned); return data;
        }
        public Node Snapshot(string path) { visited = 0; return SnapshotNode(TempPath(path), ".", 0); }
        Node SnapshotNode(string path, string name, int depth) {
            if (depth > 16 || ++visited > 2048) throw new IOException("Fixture snapshot traversal limit exceeded.");
            using (Pin pin = PinPath(path, GenericRead, true)) {
                Node node = new Node { Name = name, Exists = pin.Leaf != null };
                if (!node.Exists) return node;
                Info info = Information(pin.Leaf);
                byte[] identity = new byte[24];
                if (!GetFileInformationByHandleEx(pin.Leaf, 18, identity, (uint)identity.Length)) throw LastError("FileIdInfo");
                node.Identity = Hex(identity); node.Attributes = info.Attributes; node.Links = info.Links;
                node.CreationTime = info.Created.Value; node.LastWriteTime = info.Written.Value;
                node.Size = ((ulong)info.SizeHigh << 32) | info.SizeLow;
                node.Security = Security(pin.Leaf).GetSddlForm(AccessControlSections.Owner | AccessControlSections.Group | AccessControlSections.Access);
                if ((info.Attributes & ReparseAttribute) != 0) {
                    node.ReparseData = Convert.ToBase64String(ReparseBytes(pin.Leaf)); return node;
                }
                if ((info.Attributes & DirectoryAttribute) != 0) {
                    string[] entries = Directory.GetFileSystemEntries(path);
                    if (entries.Length > 2048) throw new IOException("Fixture snapshot entry limit exceeded.");
                    Array.Sort(entries, StringComparer.Ordinal);
                    var children = new List<Node>();
                    foreach (string child in entries) children.Add(SnapshotNode(TempPath(child), System.IO.Path.GetFileName(child), depth + 1));
                    node.Entries = children.ToArray();
                } else {
                    if (node.Size > 1048576) throw new IOException("Fixture snapshot content limit exceeded.");
                    using (var borrowed = new SafeFileHandle(pin.Leaf.DangerousGetHandle(), false))
                    using (var stream = new FileStream(borrowed, FileAccess.Read))
                    using (var hash = SHA256.Create()) node.ContentSHA256 = Hex(hash.ComputeHash(stream));
                }
                Info after = Information(pin.Leaf);
                if (info.Written.Value != after.Written.Value || info.SizeHigh != after.SizeHigh || info.SizeLow != after.SizeLow)
                    throw new IOException("Fixture changed during snapshot.");
                return node;
            }
        }
        void SetDirectoryDacl(SafeFileHandle handle, bool broaden) {
            string user = CurrentUser();
            string sddl = "D:P(A;OICI;FA;;;" + user + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)" + (broaden ? "(A;OICI;GR;;;AU)" : "");
            RawAcl acl = new RawSecurityDescriptor(sddl).DiscretionaryAcl;
            byte[] raw = new byte[acl.BinaryLength]; acl.GetBinaryForm(raw, 0);
            uint error = SetSecurityInfo(handle, 1, 0x80000004, null, IntPtr.Zero, raw, IntPtr.Zero);
            if (error != 0) throw new Win32Exception((int)error, "Set protected fixture DACL");
        }
        public void Broaden(string path) {
            using (Pin pin = PinPath(TempPath(path), ReadAttributes | WriteDac, false)) {
                OrdinaryDirectory(pin); SetDirectoryDacl(pin.Leaf, true);
            }
        }
        static byte[] JunctionBytes(string target) {
            byte[] substitute = Encoding.Unicode.GetBytes("\\??\\" + target), print = Encoding.Unicode.GetBytes(target);
            byte[] data = new byte[20 + substitute.Length + print.Length];
            if (data.Length > 16384) throw new IOException("Junction target is too long.");
            using (var writer = new BinaryWriter(new MemoryStream(data))) {
                writer.Write(MountPointTag); writer.Write((ushort)(data.Length - 8)); writer.Write((ushort)0);
                writer.Write((ushort)0); writer.Write((ushort)substitute.Length);
                writer.Write((ushort)(substitute.Length + 2)); writer.Write((ushort)print.Length);
                writer.Write(substitute); writer.Write((ushort)0); writer.Write(print); writer.Write((ushort)0);
            }
            return data;
        }
        public void Junction(string path, string target) {
            path = TempPath(path); target = TempPath(target);
            using (Pin destination = PinPath(path, ReadAttributes, true))
            using (Pin source = PinPath(target, ReadAttributes, false)) {
                OrdinaryDirectory(source);
                if (destination.Leaf != null) throw new IOException("Junction destination already exists.");
                if (!CreateDirectoryW(path, IntPtr.Zero)) throw LastError("Create junction directory");
                using (Pin link = PinPath(path, 0x40000000 | ReadAttributes, false)) {
                    OrdinaryDirectory(link); uint returned; byte[] data = JunctionBytes(target);
                    if (!DeviceIoControl(link.Leaf, SetReparse, data, (uint)data.Length, null, 0, out returned, IntPtr.Zero)) throw LastError("FSCTL_SET_REPARSE_POINT");
                    if (!Hex(ReparseBytes(link.Leaf)).Equals(Hex(data), StringComparison.Ordinal)) throw new IOException("Junction readback differs.");
                }
            }
        }
        public void RemoveJunction(string path, string target) {
            path = TempPath(path); target = TempPath(target);
            using (Pin source = PinPath(target, ReadAttributes, false))
            using (Pin link = PinPath(path, ReadAttributes | Delete, false)) {
                OrdinaryDirectory(source);
                if ((Information(link.Leaf).Attributes & ReparseAttribute) == 0 ||
                    !Hex(ReparseBytes(link.Leaf)).Equals(Hex(JunctionBytes(target)), StringComparison.Ordinal))
                    throw new IOException("Refusing removal of a different fixture object.");
                if (!SetFileInformationByHandle(link.Leaf, 4, new byte[] { 1, 0, 0, 0 }, 4)) throw LastError("Delete original junction object");
            }
        }
        public OwnerAttempt WrongOwner(string path) {
            path = TempPath(path);
            if (!path.Equals(temp + "wrong-owner-fixture", StringComparison.OrdinalIgnoreCase)) throw new IOException("WrongOwner requires its exact private leaf.");
            using (Pin pin = PinPath(path, ReadAttributes | WriteDac | WriteOwner, false)) {
                OrdinaryDirectory(pin); SetDirectoryDacl(pin.Leaf, false);
                var sid = new SecurityIdentifier("S-1-5-32-545");
                var result = new OwnerAttempt { OwnerBefore = Security(pin.Leaf).Owner.Value, RequestedOwner = sid.Value, Attempts = 1 };
                byte[] bytes = new byte[sid.BinaryLength]; sid.GetBinaryForm(bytes, 0);
                // Exactly one ordinary-token attempt. No privilege adjustment,
                // elevation, fallback owner, or policy change is performed.
                uint error = SetSecurityInfo(pin.Leaf, 1, 1, bytes, IntPtr.Zero, null, IntPtr.Zero);
                result.OwnerAfter = Security(pin.Leaf).Owner.Value;
                if (error == 0) {
                    if (result.OwnerAfter != sid.Value) throw new IOException("Successful owner assignment did not read back.");
                    result.Assigned = true; result.Failure = "";
                } else {
                    if (result.OwnerAfter != result.OwnerBefore) throw new IOException("Refused owner assignment still changed the owner.");
                    result.Assigned = false; result.Failure = "Win32 " + error + ": " + new Win32Exception((int)error).Message;
                }
                return result;
            }
        }
        public ActorObservation Handles(uint actorPid) {
            using (SafeProcessHandle handle = OpenProcess(0x00101000, false, actorPid)) {
                if (handle.IsInvalid) throw LastError("Open fixture actor for observation");
                if (WaitForSingleObject(handle, 0) != 258) throw new IOException("Fixture actor is not live.");
                uint length = 32768; var image = new StringBuilder((int)length);
                if (!QueryFullProcessImageNameW(handle, 0, image, ref length)) throw LastError("Query fixture actor image");
                string imagePath = Canonical(image.ToString());
                if (!imagePath.Equals(root + "\\bin\\api.test.exe", StringComparison.OrdinalIgnoreCase)) throw new IOException("Actor image is outside the exact fixture binary binding.");
                FT created, exited, kernel, user; uint count;
                if (!GetProcessTimes(handle, out created, out exited, out kernel, out user)) throw LastError("Query actor creation time");
                if (!GetProcessHandleCount(handle, out count) || count == 0) throw LastError("GetProcessHandleCount");
                return new ActorObservation { ActorPid = actorPid, CreationTimeFILETIME = created.Value, ExecutablePath = imagePath, HandleCount = count };
            }
        }
        public void WriteProof(string leaf, string json) {
            if (leaf != System.IO.Path.GetFileName(leaf) || !leaf.EndsWith(".json", StringComparison.Ordinal)) throw new IOException("Invalid proof leaf.");
            using (Pin parent = PinPath(root + "\\proof", ReadAttributes, false)) {
                OrdinaryDirectory(parent);
                using (var stream = new FileStream(System.IO.Path.Combine(parent.Path, leaf), FileMode.CreateNew, FileAccess.Write, FileShare.None))
                using (var writer = new StreamWriter(stream, new UTF8Encoding(false))) { writer.Write(json); writer.Flush(); stream.Flush(true); }
            }
        }
    }
}
'@

$fixtureRoot = $env:MCPHUB_RECOVERY_NAMESPACE_ROOT
$scope = [RecoveryNamespaceFixture.Scope]::new($fixtureRoot)
try {
    switch ($Action) {
        Snapshot {
            $snapshot = $scope.Snapshot($Path)
            $scope.WriteProof("snapshot-$([Guid]::NewGuid().ToString('N')).json", ([ordered]@{
                Path = $Path
                Snapshot = $snapshot
            } | ConvertTo-Json -Depth 50 -Compress))
            $snapshot | ConvertTo-Json -Depth 48 -Compress
        }
        Broaden { $scope.Broaden($Path) }
        Junction { $scope.Junction($Path, $Target) }
        RemoveJunction { $scope.RemoveJunction($Path, $Target) }
        WrongOwner {
            $attempt = $scope.WrongOwner($Path)
            $scope.WriteProof('wrong-owner-attempt.json', ($attempt | ConvertTo-Json -Compress))
        }
        Handles {
            $observation = $scope.Handles($ActorPid)
            $scope.WriteProof("handles-$([Guid]::NewGuid().ToString('N')).json", ($observation | ConvertTo-Json -Compress))
            [Console]::Out.WriteLine($observation.HandleCount.ToString([Globalization.CultureInfo]::InvariantCulture))
        }
    }
} finally {
    $scope.Dispose()
}
