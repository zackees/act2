package runner

import("archive/tar";"bytes";"fmt";"testing";"github.com/stretchr/testify/require")

func TestHostArchiveRejectsHiddenMetadataBodylessHeader(t *testing.T) {
 for _,kind:=range []byte{tar.TypeDir,tar.TypeSymlink,tar.TypeLink} {
  t.Run(fmt.Sprintf("type-%c",kind),func(t *testing.T){
   // The decoder ignores bodyless Size; raw framing must not skip the
   // following PAX headers as though they were opaque body bytes.
   var body bytes.Buffer
   writer:=tar.NewWriter(&body)
   require.NoError(t,writer.WriteHeader(&tar.Header{Name:"bodyless",Mode:0755,Typeflag:kind,Linkname:"target"}))
   require.NoError(t,writer.Close())
   data:=body.Bytes()
   copy(data[124:136],[]byte("00000002000\x00"))
   for i:=148;i<156;i++{data[i]=' '}
   checksum:=0
   for _,value:=range data[:512]{checksum+=int(value)}
   copy(data[148:156],[]byte(fmt.Sprintf("%06o\x00 ",checksum)))
   guard:=&hostArchiveReader{source:bytes.NewReader(data),limits:hostArchiveLimits{bytes:4096,headers:1,metadataBytes:16,metadataTotal:16}}
   _,err:=tar.NewReader(guard).Next()
   require.Error(t,err,"reject before bodyless size can hide metadata/header accounting")
  })
 }
}
