import { UpdateFCMServiceJSONRequestSchema, type Project } from '@buf/pushpa_cotton.bufbuild_es/projects/v1/projects_pb'
import { create } from '@bufbuild/protobuf'
import { ConnectError } from '@connectrpc/connect'
import { CircleCheck } from 'lucide-react'
import { useState } from 'react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Card, CardDescription, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'
import { projectsService } from '@/lib/rpc'

interface FirebaseIntegrationProps {
  project: Project | null
}

const FirebaseIntegration = ({ project }: FirebaseIntegrationProps) => {
  const [fcmJson, setFcmJson] = useState(project?.fcmServiceJson || '')
  const [updating, setUpdating] = useState(false)
  const [showFirebaseConfig, setShowFirebaseConfig] = useState(false)
  const [fcmJsonError, setFcmJsonError] = useState<string | null>(null)

  // Function to validate JSON
  const validateJson = (str: string): boolean => {
    try {
      JSON.parse(str)
      return true
    } catch {
      return false
    }
  }

  const handleUpdateFCMJson = async () => {
    if (!project) return

    // Validate JSON before making the API call
    if (fcmJson.trim() !== '' && !validateJson(fcmJson)) {
      setFcmJsonError('Invalid JSON format')
      toast.error('Invalid JSON format. Please correct the JSON before saving.')
      return
    }

    try {
      setUpdating(true)
      const request = create(UpdateFCMServiceJSONRequestSchema, {
        fcmServiceJson: fcmJson,
        id: project.id
      })

      await projectsService.updateFCMServiceJSON(request)
      // Refresh the project data to reflect the updated FCM JSON value
      const updatedProjectResponse = await projectsService.get({ id: project.id })
      if (updatedProjectResponse.project) {
        // In the full component, this would update the parent project state
        // For now, we'll just close the modal
      }
      toast.success('FCM service JSON updated successfully!')
      setShowFirebaseConfig(false)
    } catch (err) {
      if (err instanceof ConnectError) {
        toast.error(err.rawMessage)
      } else {
        toast.error('An error occurred while updating FCM service JSON')
      }
    } finally {
      setUpdating(false)
    }
  }

  return (
    <>
      <Card className="p-4">
        <div className="flex items-center justify-between">
          <CardTitle className="text-lg">Firebase</CardTitle>
          {project?.fcmServiceJson && project.fcmServiceJson.trim() !== '' && (
            <div className="flex items-center">
              <div className="flex items-center justify-center w-6 h-6 rounded-full bg-green-500/20">
                <CircleCheck className="h-4 w-4 text-green-600" />
              </div>
            </div>
          )}
        </div>
        <CardDescription className="mt-2 text-sm">
          Configure your Firebase service account for push notifications
        </CardDescription>
        <Button 
          variant="outline" 
          className="mt-4"
          onClick={() => setShowFirebaseConfig(true)}
        >
          {project?.fcmServiceJson && project.fcmServiceJson.trim() !== ''
            ? 'Edit Configuration'
            : 'Configure'}
        </Button>
      </Card>

      <Dialog open={showFirebaseConfig} onOpenChange={setShowFirebaseConfig}>
        <DialogContent className="max-w-2xl max-h-[90vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>Configure Firebase</DialogTitle>
            <DialogDescription>
              Enter your Firebase service account JSON to enable push notifications
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4">
            <div>
              <Label htmlFor="fcmJson" className="mb-2">Firebase Service Account JSON</Label>
              <textarea
                id="fcmJson"
                value={fcmJson}
                onChange={(e) => {
                  const value = e.target.value
                  setFcmJson(value)
                  if (value.trim() === '') {
                    setFcmJsonError(null)
                  } else {
                    const isValid = validateJson(value)
                    setFcmJsonError(isValid ? null : 'Invalid JSON format')
                  }
                }}
                rows={8}
                className={`w-full p-3 border rounded-md font-mono text-sm
                           ${fcmJsonError ? 'border-destructive' : ''}`}
                placeholder="Paste your Firebase service account JSON here..."
              />
              {fcmJsonError && (
                <p className="text-sm text-destructive mt-1">{fcmJsonError}</p>
              )}
            </div>
            <div className="flex justify-end gap-2">
              <Button
                variant="outline"
                onClick={() => setShowFirebaseConfig(false)}
                disabled={updating}
              >
                Cancel
              </Button>
              <Button 
                onClick={handleUpdateFCMJson} 
                disabled={updating || !!fcmJsonError}
              >
                {updating ? 'Saving...' : 'Update Firebase Config'}
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
    </>
  )
}

export default FirebaseIntegration