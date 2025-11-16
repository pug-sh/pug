import type { Project } from '@buf/pushpa_cotton.bufbuild_es/projects/v1/projects_pb'
import { CircleCheck } from 'lucide-react'
import { useState } from 'react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Card, CardDescription, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'

interface ApplePushNotificationsProps {
  project?: Project | null
}

const ApplePushNotifications = (props: ApplePushNotificationsProps) => {
  const { project } = props

  const isProjectAvailable = project !== undefined && project !== null

  const [showConfiguration, setShowConfiguration] = useState(false)
  const [isConfigured, setIsConfigured] = useState(false)

  const handleConfigure = () => {
    // This would normally make an API call to configure Apple Push Notifications
    toast.success('Apple Push Notifications configured successfully!')
    setIsConfigured(true)
    setShowConfiguration(false)
  }

  return (
    <>
      <Card className="p-4">
        <div className="flex items-center justify-between">
          <CardTitle className="text-lg">Apple Push Notifications</CardTitle>
          {isConfigured && (
            <div className="flex items-center">
              <div className="flex items-center justify-center w-6 h-6 rounded-full bg-green-500/20">
                <CircleCheck className="h-4 w-4 text-green-600" />
              </div>
            </div>
          )}
        </div>
        <CardDescription className="mt-2 text-sm">
          Configure your Apple Push Notification service credentials
        </CardDescription>
        <Button
          variant="outline"
          className="mt-4"
          onClick={() => setShowConfiguration(true)}
          disabled={!isProjectAvailable}
        >
          {isConfigured ? 'Edit Configuration' : 'Configure'}
        </Button>
      </Card>

      <Dialog open={showConfiguration} onOpenChange={setShowConfiguration}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Configure Apple Push Notifications</DialogTitle>
            <DialogDescription>
              Enter your Apple Push Notification service credentials
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4">
            <div className="space-y-2">
              <label htmlFor="teamId" className="text-sm font-medium">Team ID</label>
              <input
                id="teamId"
                className="w-full p-2 border rounded"
                placeholder="Enter your Apple Team ID"
              />
            </div>
            <div className="space-y-2">
              <label htmlFor="keyId" className="text-sm font-medium">Key ID</label>
              <input
                id="keyId"
                className="w-full p-2 border rounded"
                placeholder="Enter your Key ID"
              />
            </div>
            <div className="space-y-2">
              <label htmlFor="authKey" className="text-sm font-medium">Authentication Key</label>
              <textarea
                id="authKey"
                rows={4}
                className="w-full p-2 border rounded font-mono text-sm"
                placeholder="Paste your .p8 authentication key"
              />
            </div>
            <div className="space-y-2">
              <label htmlFor="bundleId" className="text-sm font-medium">Bundle ID</label>
              <input
                id="bundleId"
                className="w-full p-2 border rounded"
                placeholder="Enter your app's Bundle ID"
              />
            </div>
            <div className="flex justify-end gap-2 pt-2">
              <Button
                variant="outline"
                onClick={() => setShowConfiguration(false)}
              >
                Cancel
              </Button>
              <Button onClick={handleConfigure}>
                Save Configuration
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
    </>
  )
}

export default ApplePushNotifications